package account

import (
	"context"
	"strconv"
	"time"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

// Service são os casos de uso do ledger. Orquestra: a regra de negócio está em Account e
// Entry; a persistência, atrás das portas Store e Tx.
type Service struct {
	store    Store
	codec    ResultCodec
	now      func() time.Time
	observer Observer
}

// Option configura o Service (functional options).
type Option func(*Service)

// WithClock troca o relógio, para testes determinísticos.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService monta os casos de uso sobre o Store e o codec de resultados da borda.
func NewService(store Store, codec ResultCodec, opts ...Option) *Service {
	s := &Service{store: store, codec: codec, now: time.Now, observer: noopObserver{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// instant devolve o instante atual em UTC e com a precisão do Postgres (microssegundos):
// assim o resultado devolvido na primeira vez é igual ao lido do banco depois, inclusive na
// repetição idempotente.
func (s *Service) instant() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// OpenAccountCommand é o pedido de abertura de conta, já validado.
type OpenAccountCommand struct {
	Key      IdempotencyKey
	Holder   HolderName
	Document Document
}

// OpenAccount abre uma conta com saldo zero. ErrAccountAlreadyExists se o CPF já tem conta.
func (s *Service) OpenAccount(ctx context.Context, cmd OpenAccountCommand) (Account, error) {
	hash := requestHash(cmd.Holder.String(), cmd.Document.Digits())
	a, outcome, err := idempotent(ctx, s, scopeOpenAccount, cmd.Key, hash, s.codec.EncodeAccount, s.codec.DecodeAccount,
		func(tx Tx) (Account, error) {
			id, err := NewID()
			if err != nil {
				return Account{}, err
			}
			a := Open(id, cmd.Holder, cmd.Document, s.instant())
			if err := tx.CreateAccount(ctx, a); err != nil {
				return Account{}, err
			}
			return a, nil
		})
	s.finish(ctx, WriteEvent{Operation: scopeOpenAccount, AccountID: a.ID()}, outcome, err)
	return a, err
}

// CreditCommand é o pedido de crédito, já validado.
type CreditCommand struct {
	Key         IdempotencyKey
	AccountID   ID
	Amount      Money
	Reason      CreditReason
	Description Description
}

// Credit credita a conta e devolve a perna do cliente no lançamento.
func (s *Service) Credit(ctx context.Context, cmd CreditCommand) (Posting, error) {
	hash := requestHash(cmd.AccountID.String(), strconv.FormatInt(cmd.Amount.Cents(), 10),
		strconv.Itoa(int(cmd.Reason)), cmd.Description.String())
	p, outcome, err := idempotent(ctx, s, scopeCredit, cmd.Key, hash, s.codec.EncodePosting, s.codec.DecodePosting,
		func(tx Tx) (Posting, error) {
			return s.post(ctx, tx, cmd.AccountID, cmd.Amount, cmd.Description, func(a *Account, m Movement) (Entry, error) {
				return a.Credit(m, cmd.Reason)
			})
		})
	s.finish(ctx, WriteEvent{
		Operation: scopeCredit, Reason: cmd.Reason.String(), Amount: cmd.Amount, AccountID: cmd.AccountID,
	}, outcome, err)
	return p, err
}

// DebitCommand é o pedido de débito, já validado.
type DebitCommand struct {
	Key         IdempotencyKey
	AccountID   ID
	Amount      Money
	Reason      DebitReason
	Description Description
}

// Debit debita a conta e devolve a perna do cliente no lançamento. ErrInsufficientFunds se
// o saldo não cobre o valor: a recusa é gravada e repetida pela mesma chave.
func (s *Service) Debit(ctx context.Context, cmd DebitCommand) (Posting, error) {
	hash := requestHash(cmd.AccountID.String(), strconv.FormatInt(cmd.Amount.Cents(), 10),
		strconv.Itoa(int(cmd.Reason)), cmd.Description.String())
	p, outcome, err := idempotent(ctx, s, scopeDebit, cmd.Key, hash, s.codec.EncodePosting, s.codec.DecodePosting,
		func(tx Tx) (Posting, error) {
			return s.post(ctx, tx, cmd.AccountID, cmd.Amount, cmd.Description, func(a *Account, m Movement) (Entry, error) {
				return a.Debit(m, cmd.Reason)
			})
		})
	s.finish(ctx, WriteEvent{
		Operation: scopeDebit, Reason: cmd.Reason.String(), Amount: cmd.Amount, AccountID: cmd.AccountID,
	}, outcome, err)
	return p, err
}

// post é o lançamento sob lock pessimista (ADR-0004, seção 2): trava a conta, deixa o
// domínio decidir e grava lançamento e saldo na mesma transação.
func (s *Service) post(
	ctx context.Context, tx Tx, id ID, amount Money, desc Description,
	apply func(*Account, Movement) (Entry, error),
) (Posting, error) {
	a, err := tx.LockAccount(ctx, id)
	if err != nil {
		return Posting{}, err
	}
	entryID, err := NewEntryID()
	if err != nil {
		return Posting{}, err
	}
	e, err := apply(&a, Movement{EntryID: entryID, Amount: amount, Description: desc, PostedAt: s.instant()})
	if err != nil {
		return Posting{}, err
	}
	if err := tx.AppendEntry(ctx, e); err != nil {
		return Posting{}, err
	}
	if err := tx.SaveBalance(ctx, a); err != nil {
		return Posting{}, err
	}
	return e.Posting(), nil
}

// Account devolve os dados de uma conta de cliente.
func (s *Service) Account(ctx context.Context, id ID) (Account, error) {
	return s.store.Account(ctx, id)
}

// AccountByDocument devolve a conta de cliente do CPF informado.
func (s *Service) AccountByDocument(ctx context.Context, doc Document) (Account, error) {
	return s.store.AccountByDocument(ctx, doc)
}

// Balance devolve o saldo atual, lido sem lock: é o último saldo confirmado.
func (s *Service) Balance(ctx context.Context, id ID) (Money, error) {
	a, err := s.store.Account(ctx, id)
	if err != nil {
		return Money{}, err
	}
	return a.Balance(), nil
}

// StatementPage é uma página do extrato. Next é o cursor da próxima página (0 = não há).
type StatementPage struct {
	Lines []StatementLine
	Next  int64
}

// Statement devolve uma página do extrato, da linha mais recente para a mais antiga, a
// partir do cursor (0 = do início). pageSize zero usa o padrão; acima de 100 é inválido.
func (s *Service) Statement(ctx context.Context, id ID, cursor int64, pageSize int) (StatementPage, error) {
	switch {
	case pageSize == 0:
		pageSize = defaultPageSize
	case pageSize < 0 || pageSize > maxPageSize:
		return StatementPage{}, invalid("page_size", "must be between 1 and 100")
	}
	if cursor < 0 {
		return StatementPage{}, invalid("page_token", "is not valid")
	}
	// Conta inexistente é NOT_FOUND, não um extrato vazio.
	if _, err := s.store.Account(ctx, id); err != nil {
		return StatementPage{}, err
	}
	// Uma linha a mais diz se há próxima página sem uma consulta extra.
	lines, err := s.store.Statement(ctx, id, cursor, int32(pageSize+1)) //nolint:gosec // pageSize <= 100
	if err != nil {
		return StatementPage{}, err
	}
	page := StatementPage{Lines: lines}
	if len(lines) > pageSize {
		page.Lines = lines[:pageSize]
		page.Next = page.Lines[pageSize-1].Seq
	}
	return page, nil
}
