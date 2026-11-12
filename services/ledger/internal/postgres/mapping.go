package postgres

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"conta.local/ledger/internal/account"
)

// Representação textual no banco; os CHECKs das tabelas aceitam exatamente estes valores.
const (
	statusActive    = "active"
	directionDebit  = "debit"
	directionCredit = "credit"
)

func reasonToDB(r account.EntryReason) (string, error) {
	switch r {
	case account.ReasonOpening, account.ReasonExternal, account.ReasonReversal:
		return r.String(), nil
	default:
		return "", fmt.Errorf("postgres: motivo de lançamento desconhecido: %d", r)
	}
}

func reasonFromDB(s string) (account.EntryReason, error) {
	for _, r := range []account.EntryReason{account.ReasonOpening, account.ReasonExternal, account.ReasonReversal} {
		if s == r.String() {
			return r, nil
		}
	}
	return 0, fmt.Errorf("%w: motivo %q", errCorruptRow, s)
}

// referenceToDB grava "sem referência" como NULL, que o CHECK do banco aceita.
func referenceToDB(r account.ExternalReference) pgtype.Text {
	return pgtype.Text{String: r.String(), Valid: !r.IsZero()}
}

func referenceFromDB(t pgtype.Text) (account.ExternalReference, error) {
	if !t.Valid {
		return account.ExternalReference{}, nil
	}
	r, err := account.NewExternalReference(t.String)
	if err != nil {
		return account.ExternalReference{}, fmt.Errorf("%w: %s", errCorruptRow, err.Error())
	}
	return r, nil
}

func entryIDToDB(id account.EntryID) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(id), Valid: !id.IsZero()}
}

func entryIDFromDB(u pgtype.UUID) account.EntryID {
	if !u.Valid {
		return account.EntryID{}
	}
	return account.EntryID(u.Bytes)
}

func statusToDB(s account.Status) (string, error) {
	if s == account.StatusActive {
		return statusActive, nil
	}
	return "", fmt.Errorf("postgres: status de conta desconhecido: %d", s)
}

func statusFromDB(s string) (account.Status, error) {
	if s == statusActive {
		return account.StatusActive, nil
	}
	return 0, fmt.Errorf("%w: status %q", errCorruptRow, s)
}

func directionToDB(d account.Direction) (string, error) {
	switch d {
	case account.DirectionDebit:
		return directionDebit, nil
	case account.DirectionCredit:
		return directionCredit, nil
	default:
		return "", fmt.Errorf("postgres: sentido desconhecido: %d", d)
	}
}

func directionFromDB(s string) (account.Direction, error) {
	switch s {
	case directionDebit:
		return account.DirectionDebit, nil
	case directionCredit:
		return account.DirectionCredit, nil
	default:
		return 0, fmt.Errorf("%w: sentido %q", errCorruptRow, s)
	}
}

// accountRow reúne as colunas lidas de uma conta de cliente, venham de GetCustomerAccount ou
// de LockCustomerAccount (o sqlc gera um tipo por query, com os mesmos campos).
type accountRow struct {
	ID             uuid.UUID
	HolderName     pgtype.Text
	HolderDocument pgtype.Text
	Status         string
	BalanceCents   pgtype.Int8
	CreatedAt      time.Time
}

// toAccount reconstrói a conta passando cada campo pelos construtores do domínio: um dado
// gravado fora das regras vira erro aqui, em vez de circular como se fosse válido.
func (r accountRow) toAccount() (account.Account, error) {
	if !r.HolderName.Valid || !r.HolderDocument.Valid || !r.BalanceCents.Valid {
		return account.Account{}, fmt.Errorf("%w: conta %s sem campos de cliente", errCorruptRow, r.ID)
	}
	holder, err := account.NewHolderName(r.HolderName.String)
	if err != nil {
		return account.Account{}, fmt.Errorf("%w: conta %s: %s", errCorruptRow, r.ID, err.Error())
	}
	doc, err := account.ParseCPF(r.HolderDocument.String)
	if err != nil {
		return account.Account{}, fmt.Errorf("%w: conta %s: %s", errCorruptRow, r.ID, err.Error())
	}
	status, err := statusFromDB(r.Status)
	if err != nil {
		return account.Account{}, err
	}
	return account.Restore(account.Snapshot{
		ID: account.ID(r.ID), Holder: holder, Document: doc, Status: status,
		Balance: account.MoneyFromCents(r.BalanceCents.Int64), CreatedAt: r.CreatedAt,
	}), nil
}
