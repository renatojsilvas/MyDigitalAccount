package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/postgres/sqlcgen"
)

// O Store e o Tx satisfazem as portas que o caso de uso define em account.
var (
	_ account.Store = (*Store)(nil)
	_ account.Tx    = (*Tx)(nil)
)

// Store dá acesso ao banco do ledger. Leituras simples são métodos do Store; tudo que precisa
// de transação (travar a conta, lançar, idempotência) só existe em Tx, obtido por WithTx.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewStore cria o Store sobre um pool já aberto (o dono do pool é o main).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: sqlcgen.New(pool)}
}

// WithTx executa fn numa transação: commit se fn devolver nil, rollback caso contrário ou se
// fn entrar em panic (o panic segue adiante depois do rollback). Erros transitórios do
// Postgres, inclusive no COMMIT, voltam como RetryableError.
//
// Isolamento READ COMMITTED (o padrão): com o lock pessimista na conta, cada comando enxerga
// o que já foi confirmado, e é isso que o débito precisa (ADR-0004, seção 2).
func (s *Store) WithTx(ctx context.Context, fn func(account.Tx) error) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return classify(fmt.Errorf("postgres: iniciando transação: %w", err))
	}
	defer func() {
		// Sem cancelamento: o rollback precisa rodar mesmo se ctx já expirou.
		rctx := context.WithoutCancel(ctx)
		if p := recover(); p != nil {
			_ = tx.Rollback(rctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(rctx)
		}
	}()

	if err := fn(&Tx{q: s.q.WithTx(tx)}); err != nil {
		return err
	}
	// As constraints adiadas (partida dobrada) são conferidas aqui.
	if err := tx.Commit(ctx); err != nil {
		return classify(fmt.Errorf("postgres: commit: %w", err))
	}
	return nil
}

// Account lê uma conta de cliente, sem travar. ErrAccountNotFound se não existir.
func (s *Store) Account(ctx context.Context, id account.ID) (account.Account, error) {
	row, err := s.q.GetCustomerAccount(ctx, uuid.UUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return account.Account{}, account.ErrAccountNotFound
	}
	if err != nil {
		return account.Account{}, classify(fmt.Errorf("postgres: lendo conta %s: %w", id, err))
	}
	return accountRow(row).toAccount()
}

// AccountByDocument lê a conta de cliente do CPF, sem travar. ErrAccountNotFound se não existir.
func (s *Store) AccountByDocument(ctx context.Context, doc account.Document) (account.Account, error) {
	row, err := s.q.GetCustomerAccountByDocument(ctx, doc.Digits())
	if errors.Is(err, pgx.ErrNoRows) {
		return account.Account{}, account.ErrAccountNotFound
	}
	if err != nil {
		// O CPF não entra na mensagem: erros chegam ao log.
		return account.Account{}, classify(fmt.Errorf("postgres: lendo conta por documento %s: %w", doc.Masked(), err))
	}
	return accountRow(row).toAccount()
}

// Statement lê até limit linhas do extrato, da mais recente para a mais antiga, com Seq
// menor que before (0 = desde a mais recente). Não confere se a conta existe.
func (s *Store) Statement(ctx context.Context, id account.ID, before int64, limit int32) ([]account.StatementLine, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	rows, err := s.q.ListStatement(ctx, sqlcgen.ListStatementParams{
		AccountID: uuid.UUID(id), BeforeID: before, PageSize: limit,
	})
	if err != nil {
		return nil, classify(fmt.Errorf("postgres: lendo extrato de %s: %w", id, err))
	}
	lines := make([]account.StatementLine, 0, len(rows))
	for _, r := range rows {
		dir, err := directionFromDB(r.Direction)
		if err != nil {
			return nil, err
		}
		desc, err := account.NewDescription(r.Description)
		if err != nil {
			return nil, fmt.Errorf("%w: lançamento %s: %s", errCorruptRow, r.EntryID, err.Error())
		}
		if !r.BalanceAfterCents.Valid {
			return nil, fmt.Errorf("%w: perna %d de conta de cliente sem saldo", errCorruptRow, r.ID)
		}
		lines = append(lines, account.StatementLine{Seq: r.ID, Posting: account.Posting{
			EntryID: account.EntryID(r.EntryID), AccountID: id, Direction: dir,
			Amount: account.MoneyFromCents(r.AmountCents), BalanceAfter: account.MoneyFromCents(r.BalanceAfterCents.Int64),
			Description: desc, PostedAt: r.PostedAt,
		}})
	}
	return lines, nil
}

// NetPostings recalcula, das pernas, créditos menos débitos de qualquer conta (de cliente ou
// interna). Para conta de cliente, deve ser igual ao saldo gravado.
func (s *Store) NetPostings(ctx context.Context, id account.ID) (account.Money, error) {
	net, err := s.q.NetPostings(ctx, uuid.UUID(id))
	if err != nil {
		return account.Money{}, classify(fmt.Errorf("postgres: somando pernas de %s: %w", id, err))
	}
	return account.MoneyFromCents(net), nil
}
