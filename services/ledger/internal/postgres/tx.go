package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/postgres/sqlcgen"
)

// Tx são as operações que só fazem sentido dentro de uma transação. Só existe dentro de
// Store.WithTx: travar uma conta fora de transação não compila.
type Tx struct {
	q *sqlcgen.Queries
}

// CreateAccount grava uma conta nova. ErrAccountAlreadyExists se o CPF já tem conta; nesse
// caso a transação continua utilizável, para gravar a recusa (ADR-0004, seção 4).
func (t *Tx) CreateAccount(ctx context.Context, a account.Account) error {
	s := a.Snapshot()
	status, err := statusToDB(s.Status)
	if err != nil {
		return err
	}
	_, err = t.q.InsertCustomerAccount(ctx, sqlcgen.InsertCustomerAccountParams{
		ID:             uuid.UUID(s.ID),
		HolderName:     pgtype.Text{String: s.Holder.String(), Valid: true},
		HolderDocument: pgtype.Text{String: s.Document.Digits(), Valid: true},
		Status:         status,
		CreatedAt:      s.CreatedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return account.ErrAccountAlreadyExists
	}
	if err != nil {
		return classify(fmt.Errorf("postgres: criando conta %s: %w", s.ID, err))
	}
	return nil
}

// LockAccount lê a conta de cliente com SELECT ... FOR UPDATE: até o fim da transação,
// nenhum outro lançamento muda esse saldo. Se outra transação já tem o lock, espera até o
// lock_timeout da sessão e então devolve RetryableError. ErrAccountNotFound se não existir.
func (t *Tx) LockAccount(ctx context.Context, id account.ID) (account.Account, error) {
	row, err := t.q.LockCustomerAccount(ctx, uuid.UUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return account.Account{}, account.ErrAccountNotFound
	}
	if err != nil {
		return account.Account{}, classify(fmt.Errorf("postgres: travando conta %s: %w", id, err))
	}
	return accountRow(row).toAccount()
}

// SaveBalance grava o saldo atual da conta (que deve ter sido travada nesta transação).
func (t *Tx) SaveBalance(ctx context.Context, a account.Account) error {
	n, err := t.q.UpdateCustomerBalance(ctx, sqlcgen.UpdateCustomerBalanceParams{
		ID: uuid.UUID(a.ID()), BalanceCents: pgtype.Int8{Int64: a.Balance().Cents(), Valid: true},
	})
	if err != nil {
		return classify(fmt.Errorf("postgres: gravando saldo de %s: %w", a.ID(), err))
	}
	if n != 1 {
		return fmt.Errorf("postgres: gravando saldo de %s: %d linhas afetadas, esperava 1", a.ID(), n)
	}
	return nil
}

// AppendEntry grava o lançamento e as suas pernas. Só a perna da conta de cliente leva o
// saldo posterior; a partida dobrada é conferida de novo pelo banco no COMMIT.
func (t *Tx) AppendEntry(ctx context.Context, e account.Entry) error {
	err := t.q.InsertJournalEntry(ctx, sqlcgen.InsertJournalEntryParams{
		ID: uuid.UUID(e.ID()), Description: e.Description().String(), PostedAt: e.PostedAt(),
	})
	if err != nil {
		return classify(fmt.Errorf("postgres: gravando lançamento %s: %w", e.ID(), err))
	}
	posting := e.Posting()
	for _, leg := range e.Legs() {
		dir, err := directionToDB(leg.Direction)
		if err != nil {
			return err
		}
		var balanceAfter pgtype.Int8
		if leg.AccountID == posting.AccountID {
			balanceAfter = pgtype.Int8{Int64: posting.BalanceAfter.Cents(), Valid: true}
		}
		err = t.q.InsertPosting(ctx, sqlcgen.InsertPostingParams{
			EntryID: uuid.UUID(e.ID()), AccountID: uuid.UUID(leg.AccountID), Direction: dir,
			AmountCents: leg.Amount.Cents(), BalanceAfterCents: balanceAfter,
		})
		if err != nil {
			return classify(fmt.Errorf("postgres: gravando perna de %s em %s: %w", e.ID(), leg.AccountID, err))
		}
	}
	return nil
}

// ClaimKey reserva a chave de idempotência; deve ser o primeiro comando da transação.
// Devolve true se a chave é nova. Se outra transação reservou a mesma chave e ainda não
// terminou, espera por ela: se ela fizer commit, devolve false (a chave existe); se fizer
// rollback, a reserva passa para esta transação e devolve true.
func (t *Tx) ClaimKey(ctx context.Context, scope, key string, requestHash []byte) (bool, error) {
	_, err := t.q.ClaimIdempotencyKey(ctx, sqlcgen.ClaimIdempotencyKeyParams{
		Scope: scope, Key: key, RequestHash: requestHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, classify(fmt.Errorf("postgres: reservando chave %s/%s: %w", scope, key, err))
	}
	return true, nil
}

// KeyRecord lê o registro de uma chave de idempotência.
func (t *Tx) KeyRecord(ctx context.Context, scope, key string) (account.IdempotencyRecord, error) {
	row, err := t.q.GetIdempotencyKey(ctx, sqlcgen.GetIdempotencyKeyParams{Scope: scope, Key: key})
	if err != nil {
		return account.IdempotencyRecord{}, classify(fmt.Errorf("postgres: lendo chave %s/%s: %w", scope, key, err))
	}
	return account.IdempotencyRecord{
		RequestHash: row.RequestHash,
		Completed:   row.StatusCode.Valid,
		StatusCode:  int32(row.StatusCode.Int16),
		Body:        row.Body,
	}, nil
}

// CompleteKey grava o resultado de uma chave reservada nesta transação. Falha se a chave
// não estiver pendente: gravar duas vezes o resultado seria bug de quem chama.
func (t *Tx) CompleteKey(ctx context.Context, scope, key string, statusCode int32, body []byte) error {
	if statusCode < 0 || statusCode > 16 {
		return fmt.Errorf("postgres: código de status gRPC inválido: %d", statusCode)
	}
	n, err := t.q.CompleteIdempotencyKey(ctx, sqlcgen.CompleteIdempotencyKeyParams{
		Scope: scope, Key: key, StatusCode: pgtype.Int2{Int16: int16(statusCode), Valid: true}, Body: body,
	})
	if err != nil {
		return classify(fmt.Errorf("postgres: completando chave %s/%s: %w", scope, key, err))
	}
	if n != 1 {
		return fmt.Errorf("postgres: chave %s/%s não estava pendente", scope, key)
	}
	return nil
}
