package account

import (
	"fmt"
	"time"
)

// Status é a situação da conta.
type Status int

// Situações da conta.
const (
	StatusActive Status = iota + 1
)

// Account é uma conta de cliente. O saldo é materializado e só muda pelos métodos Credit e
// Debit, que produzem o lançamento correspondente (ADR-0004, seção 2).
type Account struct {
	id        ID
	holder    HolderName
	document  Document
	status    Status
	balance   Money
	createdAt time.Time
}

// Snapshot é o estado de uma conta, usado para persistir e reconstruir sem expor os campos.
type Snapshot struct {
	ID        ID
	Holder    HolderName
	Document  Document
	Status    Status
	Balance   Money
	CreatedAt time.Time
}

// Open cria uma conta ativa com saldo zero. Os argumentos já são objetos de valor
// validados, por isso não há erro possível aqui.
func Open(id ID, holder HolderName, doc Document, now time.Time) Account {
	return Account{id: id, holder: holder, document: doc, status: StatusActive, createdAt: now}
}

// Restore reconstrói uma conta a partir do estado persistido.
func Restore(s Snapshot) Account {
	return Account{
		id: s.ID, holder: s.Holder, document: s.Document,
		status: s.Status, balance: s.Balance, createdAt: s.CreatedAt,
	}
}

// Snapshot devolve o estado atual da conta.
func (a Account) Snapshot() Snapshot {
	return Snapshot{
		ID: a.id, Holder: a.holder, Document: a.document,
		Status: a.status, Balance: a.balance, CreatedAt: a.createdAt,
	}
}

// ID devolve o identificador da conta.
func (a Account) ID() ID { return a.id }

// Balance devolve o saldo atual.
func (a Account) Balance() Money { return a.balance }

// Credit aumenta o saldo e devolve o lançamento: débito na conta interna do motivo,
// crédito nesta conta.
func (a *Account) Credit(m Movement, reason CreditReason) (Entry, error) {
	counter, err := reason.counterpart()
	if err != nil {
		return Entry{}, err
	}
	balance, err := a.balance.Add(m.Amount)
	if err != nil {
		return Entry{}, fmt.Errorf("credit account %s: %w", a.id, err)
	}
	return a.apply(m, DirectionCredit, balance, reason.entryReason(), EntryID{},
		Leg{AccountID: counter, Direction: DirectionDebit, Amount: m.Amount},
		Leg{AccountID: a.id, Direction: DirectionCredit, Amount: m.Amount},
	)
}

// Debit diminui o saldo e devolve o lançamento: débito nesta conta, crédito na conta
// interna do motivo. Recusa com ErrInsufficientFunds se o saldo não cobre o valor.
func (a *Account) Debit(m Movement, reason DebitReason) (Entry, error) {
	counter, err := reason.counterpart()
	if err != nil {
		return Entry{}, err
	}
	if a.balance.LessThan(m.Amount) {
		return Entry{}, ErrInsufficientFunds
	}
	balance, err := a.balance.Sub(m.Amount)
	if err != nil {
		return Entry{}, fmt.Errorf("debit account %s: %w", a.id, err)
	}
	return a.apply(m, DirectionDebit, balance, ReasonExternal, EntryID{},
		Leg{AccountID: a.id, Direction: DirectionDebit, Amount: m.Amount},
		Leg{AccountID: counter, Direction: DirectionCredit, Amount: m.Amount},
	)
}

// Reverse estorna o lançamento original (ADR-0009): grava um lançamento novo, de mesmo valor,
// com as pernas invertidas. Só um débito EXTERNAL desta conta é estornável; o valor de m é
// ignorado, porque o estorno é sempre integral. Quem chama garante que o original ainda não foi
// estornado (a checagem precisa do banco, sob o lock desta conta).
func (a *Account) Reverse(m Movement, original StoredEntry) (Entry, error) {
	if original.Reason != ReasonExternal || original.CustomerAccountID != a.id {
		return Entry{}, ErrEntryNotReversible
	}
	legs := make([]Leg, 0, len(original.Legs))
	var amount Money
	for _, l := range original.Legs {
		inverted := l
		switch l.Direction {
		case DirectionDebit:
			inverted.Direction = DirectionCredit
		case DirectionCredit:
			inverted.Direction = DirectionDebit
		default:
			return Entry{}, fmt.Errorf("%w: unknown direction %d", ErrInvalidEntry, l.Direction)
		}
		if l.AccountID == a.id {
			if l.Direction != DirectionDebit {
				return Entry{}, ErrEntryNotReversible
			}
			amount = l.Amount
		}
		legs = append(legs, inverted)
	}
	if amount.Cents() <= 0 {
		return Entry{}, fmt.Errorf("%w: entry %s has no leg in account %s", ErrInvalidEntry, original.ID, a.id)
	}
	balance, err := a.balance.Add(amount)
	if err != nil {
		return Entry{}, fmt.Errorf("reverse in account %s: %w", a.id, err)
	}
	m.Amount = amount
	return a.apply(m, DirectionCredit, balance, ReasonReversal, original.ID, legs...)
}

// apply valida o lançamento e só então muda o saldo: se a partida dobrada falhar, a conta
// fica exatamente como estava.
func (a *Account) apply(
	m Movement, dir Direction, balance Money, reason EntryReason, reverses EntryID, legs ...Leg,
) (Entry, error) {
	posting := Posting{
		EntryID: m.EntryID, AccountID: a.id, Direction: dir, Amount: m.Amount,
		BalanceAfter: balance, Description: m.Description, PostedAt: m.PostedAt,
		Reason: reason, Reference: m.Reference, ReversesEntryID: reverses,
	}
	entry, err := newEntry(m, posting, legs...)
	if err != nil {
		return Entry{}, err
	}
	a.balance = balance
	return entry, nil
}
