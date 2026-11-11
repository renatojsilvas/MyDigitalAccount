package account

import (
	"fmt"
	"time"
)

// Direction é o sentido de uma perna. Numa conta de cliente (passivo do banco), crédito
// aumenta o saldo e débito diminui.
type Direction int

// Sentidos de uma perna.
const (
	DirectionDebit Direction = iota + 1
	DirectionCredit
)

// Leg é uma perna do lançamento: uma conta, um sentido e um valor positivo.
type Leg struct {
	AccountID ID
	Direction Direction
	Amount    Money
}

// Posting é a perna do lançamento vista da conta de cliente, como aparece no extrato.
type Posting struct {
	EntryID      EntryID
	AccountID    ID
	Direction    Direction
	Amount       Money
	BalanceAfter Money
	Description  Description
	PostedAt     time.Time
}

// Movement reúne os dados de uma movimentação que vêm de fora do domínio: identificador
// e instante (gerados pelo caso de uso), valor e descrição (validados na entrada).
type Movement struct {
	EntryID     EntryID
	Amount      Money
	Description Description
	PostedAt    time.Time
}

// Entry é um lançamento em partida dobrada. Só é construído por newEntry, que garante a
// invariante; os campos não exportados impedem alterá-lo depois (append-only).
type Entry struct {
	id          EntryID
	description Description
	postedAt    time.Time
	legs        []Leg
	posting     Posting
}

// ID devolve o identificador do lançamento.
func (e Entry) ID() EntryID { return e.id }

// Description devolve a descrição do lançamento.
func (e Entry) Description() Description { return e.description }

// PostedAt devolve o instante do lançamento.
func (e Entry) PostedAt() time.Time { return e.postedAt }

// Legs devolve uma cópia das pernas, para que ninguém altere o lançamento por fora.
func (e Entry) Legs() []Leg {
	return append([]Leg(nil), e.legs...)
}

// Posting devolve a perna da conta de cliente, com o saldo logo depois dela.
func (e Entry) Posting() Posting { return e.posting }

// newEntry valida a partida dobrada: ao menos duas pernas, valores positivos, sentidos
// conhecidos, uma conta por perna e soma dos débitos igual à soma dos créditos.
func newEntry(m Movement, posting Posting, legs ...Leg) (Entry, error) {
	if len(legs) < 2 {
		return Entry{}, fmt.Errorf("%w: needs at least two legs, got %d", ErrInvalidEntry, len(legs))
	}
	var debits, credits Money
	seen := make(map[ID]bool, len(legs))
	for _, l := range legs {
		if l.Amount.Cents() <= 0 {
			return Entry{}, fmt.Errorf("%w: leg amount must be positive", ErrInvalidEntry)
		}
		if seen[l.AccountID] {
			return Entry{}, fmt.Errorf("%w: account %s appears in more than one leg", ErrInvalidEntry, l.AccountID)
		}
		seen[l.AccountID] = true

		var err error
		switch l.Direction {
		case DirectionDebit:
			debits, err = debits.Add(l.Amount)
		case DirectionCredit:
			credits, err = credits.Add(l.Amount)
		default:
			return Entry{}, fmt.Errorf("%w: unknown direction %d", ErrInvalidEntry, l.Direction)
		}
		if err != nil {
			return Entry{}, fmt.Errorf("%w: %w", ErrInvalidEntry, err)
		}
	}
	if debits != credits {
		return Entry{}, fmt.Errorf("%w: debits %d != credits %d", ErrInvalidEntry, debits.Cents(), credits.Cents())
	}
	return Entry{
		id:          m.EntryID,
		description: m.Description,
		postedAt:    m.PostedAt,
		legs:        append([]Leg(nil), legs...),
		posting:     posting,
	}, nil
}
