package account_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"conta.local/ledger/internal/account"
)

var (
	customerID = account.ID{0: 0xaa, 15: 0x01}
	entryID    = account.EntryID{0: 0xee, 15: 0x01}
	openedAt   = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	postedAt   = openedAt.Add(time.Hour)
)

// accountWith devolve uma conta de cliente com o saldo informado, como se lida do banco.
func accountWith(t *testing.T, balance int64) account.Account {
	t.Helper()
	holder, err := account.NewHolderName("Maria Silva")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := account.ParseCPF("11144477735")
	if err != nil {
		t.Fatal(err)
	}
	s := account.Open(customerID, holder, doc, openedAt).Snapshot()
	s.Balance = account.MoneyFromCents(balance)
	return account.Restore(s)
}

func movement(t *testing.T, cents int64) account.Movement {
	t.Helper()
	amount, err := account.NewAmount(cents)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := account.NewDescription("teste")
	if err != nil {
		t.Fatal(err)
	}
	return account.Movement{EntryID: entryID, Amount: amount, Description: desc, PostedAt: postedAt}
}

// assertEntry confere a partida dobrada e a perna do cliente de um lançamento produzido.
func assertEntry(t *testing.T, e account.Entry, wantCustomerDir account.Direction, wantCounter account.ID, amount, balanceAfter int64) {
	t.Helper()
	legs := e.Legs()
	if len(legs) != 2 {
		t.Fatalf("len(legs) = %d, want 2", len(legs))
	}
	var debits, credits int64
	byAccount := map[account.ID]account.Leg{}
	for _, l := range legs {
		byAccount[l.AccountID] = l
		if l.Direction == account.DirectionDebit {
			debits += l.Amount.Cents()
		} else {
			credits += l.Amount.Cents()
		}
	}
	if debits != credits || debits != amount {
		t.Errorf("débitos %d, créditos %d, want ambos %d", debits, credits, amount)
	}
	if got := byAccount[customerID].Direction; got != wantCustomerDir {
		t.Errorf("sentido na conta do cliente = %d, want %d", got, wantCustomerDir)
	}
	if _, ok := byAccount[wantCounter]; !ok {
		t.Errorf("contrapartida = %v, want perna em %s", legs, wantCounter)
	}

	p := e.Posting()
	if p.EntryID != entryID || p.AccountID != customerID || p.Direction != wantCustomerDir ||
		p.Amount.Cents() != amount || p.BalanceAfter.Cents() != balanceAfter ||
		p.Description.String() != "teste" || !p.PostedAt.Equal(postedAt) {
		t.Errorf("posting = %+v", p)
	}
	if e.ID() != entryID || !e.PostedAt().Equal(postedAt) || e.Description().String() != "teste" {
		t.Errorf("entry = id %s, postedAt %s, descrição %q", e.ID(), e.PostedAt(), e.Description())
	}
}

func TestOpen(t *testing.T) {
	a := accountWith(t, 0)
	s := a.Snapshot()
	if s.ID != customerID || s.Status != account.StatusActive || s.Balance.Cents() != 0 ||
		!s.CreatedAt.Equal(openedAt) || s.Holder.String() != "Maria Silva" || s.Document.Digits() != "11144477735" {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestDebit(t *testing.T) {
	tests := []struct {
		name        string
		balance     int64
		amount      int64
		reason      account.DebitReason
		wantBalance int64
		wantErr     error
		wantInvalid bool
	}{
		{name: "parcial", balance: 10_000, amount: 3_000, reason: account.DebitExternal, wantBalance: 7_000},
		{name: "saldo exato zera", balance: 10_000, amount: 10_000, reason: account.DebitExternal, wantBalance: 0},
		{name: "um centavo além do saldo", balance: 10_000, amount: 10_001, reason: account.DebitExternal, wantErr: account.ErrInsufficientFunds},
		{name: "conta zerada", balance: 0, amount: 1, reason: account.DebitExternal, wantErr: account.ErrInsufficientFunds},
		{name: "motivo desconhecido", balance: 10_000, amount: 1, reason: 0, wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := accountWith(t, tt.balance)
			e, err := a.Debit(movement(t, tt.amount), tt.reason)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantInvalid:
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "reason" {
					t.Fatalf("err = %v, want ValidationError em reason", err)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				assertEntry(t, e, account.DirectionDebit, account.SettlementAccountID, tt.amount, tt.wantBalance)
				if a.Balance().Cents() != tt.wantBalance {
					t.Errorf("saldo = %d, want %d", a.Balance().Cents(), tt.wantBalance)
				}
				return
			}
			if a.Balance().Cents() != tt.balance {
				t.Errorf("recusa alterou o saldo: %d, want %d", a.Balance().Cents(), tt.balance)
			}
		})
	}
}

func TestCredit(t *testing.T) {
	tests := []struct {
		name        string
		balance     int64
		amount      int64
		reason      account.CreditReason
		wantCounter account.ID
		wantBalance int64
		wantErr     error
		wantInvalid bool
	}{
		{name: "saldo inicial vai contra opening_balance", balance: 0, amount: 50_000, reason: account.CreditOpening, wantCounter: account.OpeningBalanceAccountID, wantBalance: 50_000},
		{name: "externo vai contra settlement", balance: 1_000, amount: 500, reason: account.CreditExternal, wantCounter: account.SettlementAccountID, wantBalance: 1_500},
		{name: "estouro de int64", balance: math.MaxInt64, amount: 1, reason: account.CreditExternal, wantErr: account.ErrMoneyOverflow},
		{name: "motivo desconhecido", balance: 0, amount: 1, reason: 99, wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := accountWith(t, tt.balance)
			e, err := a.Credit(movement(t, tt.amount), tt.reason)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.wantInvalid:
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "reason" {
					t.Fatalf("err = %v, want ValidationError em reason", err)
				}
			default:
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				assertEntry(t, e, account.DirectionCredit, tt.wantCounter, tt.amount, tt.wantBalance)
				if a.Balance().Cents() != tt.wantBalance {
					t.Errorf("saldo = %d, want %d", a.Balance().Cents(), tt.wantBalance)
				}
				return
			}
			if a.Balance().Cents() != tt.balance {
				t.Errorf("recusa alterou o saldo: %d, want %d", a.Balance().Cents(), tt.balance)
			}
		})
	}
}

// Um Movement montado à mão, sem NewAmount, não pode furar a partida dobrada nem o saldo.
func TestMovementWithoutValidAmountIsRejected(t *testing.T) {
	for _, cents := range []int64{0, -500} {
		a := accountWith(t, 1_000)
		m := account.Movement{EntryID: entryID, Amount: account.MoneyFromCents(cents), PostedAt: postedAt}
		if _, err := a.Debit(m, account.DebitExternal); !errors.Is(err, account.ErrInvalidEntry) {
			t.Errorf("Debit(%d) err = %v, want ErrInvalidEntry", cents, err)
		}
		if _, err := a.Credit(m, account.CreditExternal); !errors.Is(err, account.ErrInvalidEntry) {
			t.Errorf("Credit(%d) err = %v, want ErrInvalidEntry", cents, err)
		}
		if a.Balance().Cents() != 1_000 {
			t.Errorf("saldo = %d, want 1000 intacto", a.Balance().Cents())
		}
	}
}

func TestEntryLegsIsACopy(t *testing.T) {
	a := accountWith(t, 0)
	e, err := a.Credit(movement(t, 100), account.CreditOpening)
	if err != nil {
		t.Fatal(err)
	}
	legs := e.Legs()
	legs[0].Amount = account.MoneyFromCents(999)
	if e.Legs()[0].Amount.Cents() != 100 {
		t.Fatal("alterar o slice devolvido alterou o lançamento")
	}
}

func TestInternalAccountsAreDistinct(t *testing.T) {
	if account.OpeningBalanceAccountID == account.SettlementAccountID {
		t.Fatal("contas internas precisam de identificadores distintos")
	}
}
