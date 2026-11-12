package clients

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/transfers/internal/saga"
)

// Ledger implementa saga.Ledger.
type Ledger struct {
	c   ledgerv1.LedgerServiceClient
	dep *Dependency
}

var _ saga.Ledger = (*Ledger)(nil)

// NewLedger cria o adaptador.
func NewLedger(c ledgerv1.LedgerServiceClient, dep *Dependency) *Ledger {
	return &Ledger{c: c, dep: dep}
}

var (
	accountNotFound      = ledgerv1.ErrorReason_ERROR_REASON_ACCOUNT_NOT_FOUND.String()
	insufficientFunds    = ledgerv1.ErrorReason_ERROR_REASON_INSUFFICIENT_FUNDS.String()
	entryAlreadyReversed = ledgerv1.ErrorReason_ERROR_REASON_ENTRY_ALREADY_REVERSED.String()
)

// AccountByDocument resolve a chave Pix (CPF) na conta do ledger.
func (l *Ledger) AccountByDocument(ctx context.Context, cpf string) (uuid.UUID, error) {
	var resp *ledgerv1.GetAccountByDocumentResponse
	err := l.dep.call(ctx, func(ctx context.Context) error {
		r, err := l.c.GetAccountByDocument(ctx, &ledgerv1.GetAccountByDocumentRequest{HolderDocument: cpf})
		resp = r
		return err
	})
	switch {
	case reasonOf(err) == accountNotFound:
		return uuid.UUID{}, saga.ErrAccountNotFound
	case err != nil:
		return uuid.UUID{}, l.dep.unavailable(err)
	}
	return parseID(resp.GetAccount().GetAccountId())
}

// Debit debita a conta de origem. A chave vem da saga (Pix + etapa): repetir é seguro.
func (l *Ledger) Debit(ctx context.Context, key string, accountID uuid.UUID, amountCents int64, reference, description string) (uuid.UUID, error) {
	var resp *ledgerv1.DebitResponse
	err := l.dep.call(ctx, func(ctx context.Context) error {
		r, err := l.c.Debit(ctx, &ledgerv1.DebitRequest{
			IdempotencyKey: key, AccountId: accountID.String(), AmountCents: amountCents,
			Reason: ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL, Description: description, ExternalReference: reference,
		})
		resp = r
		return err
	})
	switch reason := reasonOf(err); {
	case reason == insufficientFunds:
		return uuid.UUID{}, saga.ErrInsufficientFunds
	case reason == accountNotFound:
		return uuid.UUID{}, saga.ErrAccountNotFound
	case err != nil:
		return uuid.UUID{}, l.dep.unavailable(err)
	}
	return parseID(resp.GetPosting().GetEntryId())
}

// Credit credita a conta de destino (Pix recebido; contrapartida: settlement).
func (l *Ledger) Credit(ctx context.Context, key string, accountID uuid.UUID, amountCents int64, reference, description string) error {
	err := l.dep.call(ctx, func(ctx context.Context) error {
		_, err := l.c.Credit(ctx, &ledgerv1.CreditRequest{
			IdempotencyKey: key, AccountId: accountID.String(), AmountCents: amountCents,
			Reason: ledgerv1.CreditReason_CREDIT_REASON_EXTERNAL, Description: description, ExternalReference: reference,
		})
		return err
	})
	if err != nil {
		return l.dep.unavailable(err)
	}
	return nil
}

// Reverse estorna o débito (ADR-0009). ENTRY_ALREADY_REVERSED conta como sucesso: com a mesma
// chave a resposta seria a gravada, então essa recusa só aparece se o débito já foi estornado
// por outro caminho, e o estado final (dinheiro devolvido) é o correto. Tratá-la como falha
// prenderia o Pix em REVERSING para sempre.
func (l *Ledger) Reverse(ctx context.Context, key string, entryID uuid.UUID, reference, description string) error {
	err := l.dep.call(ctx, func(ctx context.Context) error {
		_, err := l.c.Reverse(ctx, &ledgerv1.ReverseRequest{
			IdempotencyKey: key, EntryId: entryID.String(), ExternalReference: reference, Description: description,
		})
		return err
	})
	if err != nil && reasonOf(err) != entryAlreadyReversed {
		return l.dep.unavailable(err)
	}
	return nil
}

func parseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("clients: resposta com ID inválido %q: %w", s, err)
	}
	return id, nil
}
