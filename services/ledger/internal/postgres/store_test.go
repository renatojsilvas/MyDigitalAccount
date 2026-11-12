package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/platform/postgres/pgtest"
	"conta.local/ledger/internal/postgres"
)

// Instantes inteiros: o Postgres guarda microssegundos, e a comparação fica exata.
var openedAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *postgres.Store {
	t.Helper()
	_, pool := pgtest.Migrated(t, 0)
	return postgres.NewStore(pool)
}

// newCustomer monta uma conta nova (ainda não gravada) com o CPF 111.444.777-35.
func newCustomer(t *testing.T) account.Account {
	t.Helper()
	id, err := account.NewID()
	if err != nil {
		t.Fatal(err)
	}
	holder, err := account.NewHolderName("Maria Silva")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := account.ParseCPF("11144477735")
	if err != nil {
		t.Fatal(err)
	}
	return account.Open(id, holder, doc, openedAt)
}

func movement(t *testing.T, cents int64, at time.Time) account.Movement {
	t.Helper()
	id, err := account.NewEntryID()
	if err != nil {
		t.Fatal(err)
	}
	amount, err := account.NewAmount(cents)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := account.NewDescription("mov")
	if err != nil {
		t.Fatal(err)
	}
	return account.Movement{EntryID: id, Amount: amount, Description: desc, PostedAt: at}
}

// create grava a conta numa transação própria.
func create(t *testing.T, s *postgres.Store, a account.Account) {
	t.Helper()
	if err := s.WithTx(t.Context(), func(tx account.Tx) error { return tx.CreateAccount(t.Context(), a) }); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
}

// post trava a conta, aplica a operação do domínio e grava lançamento e saldo, como o caso
// de uso fará no passo 4.
func post(ctx context.Context, s *postgres.Store, id account.ID, op func(*account.Account) (account.Entry, error)) error {
	return s.WithTx(ctx, func(tx account.Tx) error {
		a, err := tx.LockAccount(ctx, id)
		if err != nil {
			return err
		}
		e, err := op(&a)
		if err != nil {
			return err
		}
		if err := tx.AppendEntry(ctx, e); err != nil {
			return err
		}
		return tx.SaveBalance(ctx, a)
	})
}

func credit(t *testing.T, m account.Movement) func(*account.Account) (account.Entry, error) {
	t.Helper()
	return func(a *account.Account) (account.Entry, error) { return a.Credit(m, account.CreditOpening) }
}

func TestAccountRoundTrip(t *testing.T) {
	s := newStore(t)
	want := newCustomer(t)
	create(t, s, want)

	got, err := s.Account(t.Context(), want.ID())
	if err != nil {
		t.Fatal(err)
	}
	g, w := got.Snapshot(), want.Snapshot()
	if g.ID != w.ID || g.Holder != w.Holder || g.Document.Digits() != w.Document.Digits() ||
		g.Status != w.Status || g.Balance != w.Balance || !g.CreatedAt.Equal(w.CreatedAt) {
		t.Fatalf("lida %+v, gravada %+v", g, w)
	}
}

func TestAccountNotFound(t *testing.T) {
	s := newStore(t)
	missing := newCustomer(t).ID()

	if _, err := s.Account(t.Context(), missing); !errors.Is(err, account.ErrAccountNotFound) {
		t.Errorf("Account: err = %v, want ErrAccountNotFound", err)
	}
	err := s.WithTx(t.Context(), func(tx account.Tx) error {
		_, err := tx.LockAccount(t.Context(), missing)
		return err
	})
	if !errors.Is(err, account.ErrAccountNotFound) {
		t.Errorf("LockAccount: err = %v, want ErrAccountNotFound", err)
	}
	// Uma conta interna não é conta de cliente: não pode ser lida nem travada por aqui.
	if _, err := s.Account(t.Context(), account.SettlementAccountID); !errors.Is(err, account.ErrAccountNotFound) {
		t.Errorf("conta interna: err = %v, want ErrAccountNotFound", err)
	}
}

func TestDuplicateDocumentKeepsTransactionUsable(t *testing.T) {
	s := newStore(t)
	create(t, s, newCustomer(t))

	err := s.WithTx(t.Context(), func(tx account.Tx) error {
		if err := tx.CreateAccount(t.Context(), newCustomer(t)); !errors.Is(err, account.ErrAccountAlreadyExists) {
			t.Fatalf("err = %v, want ErrAccountAlreadyExists", err)
		}
		// A recusa precisa poder ser gravada na mesma transação.
		if _, err := tx.ClaimKey(t.Context(), "open_account", "k", make([]byte, 32)); err != nil {
			return err
		}
		return tx.CompleteKey(t.Context(), "open_account", "k", 6, []byte{1})
	})
	if err != nil {
		t.Fatalf("a transação deveria continuar utilizável: %v", err)
	}
}

func TestPostedEntriesKeepBalanceEqualToPostings(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	a := newCustomer(t)
	create(t, s, a)

	if err := post(ctx, s, a.ID(), credit(t, movement(t, 10_000, openedAt))); err != nil {
		t.Fatal(err)
	}
	debit := movement(t, 2_500, openedAt.Add(time.Minute))
	if err := post(ctx, s, a.ID(), func(acc *account.Account) (account.Entry, error) {
		return acc.Debit(debit, account.DebitExternal)
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Account(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}
	net, err := s.NetPostings(ctx, a.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.Balance().Cents() != 7_500 || net != got.Balance() {
		t.Fatalf("saldo gravado %d, soma das pernas %d; want 7500 nos dois", got.Balance().Cents(), net.Cents())
	}
	// As contrapartidas fecham com o cliente: o sistema inteiro soma zero.
	opening, _ := s.NetPostings(ctx, account.OpeningBalanceAccountID)
	settlement, _ := s.NetPostings(ctx, account.SettlementAccountID)
	if opening.Cents() != -10_000 || settlement.Cents() != 2_500 {
		t.Fatalf("opening_balance %d, settlement %d; want -10000 e 2500", opening.Cents(), settlement.Cents())
	}
	if total := net.Cents() + opening.Cents() + settlement.Cents(); total != 0 {
		t.Fatalf("soma de todas as contas = %d, want 0 (dinheiro criado ou destruído)", total)
	}

	lines, err := s.Statement(ctx, a.ID(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("extrato com %d linhas, want 2", len(lines))
	}
	last := lines[0].Posting
	if last.EntryID != debit.EntryID || last.Direction != account.DirectionDebit ||
		last.Amount.Cents() != 2_500 || last.BalanceAfter.Cents() != 7_500 ||
		last.Description.String() != "mov" || !last.PostedAt.Equal(debit.PostedAt) {
		t.Fatalf("linha mais recente = %+v", last)
	}
}

func TestStatementPagination(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	a := newCustomer(t)
	create(t, s, a)
	for i := range 5 {
		if err := post(ctx, s, a.ID(), credit(t, movement(t, int64(i+1)*100, openedAt))); err != nil {
			t.Fatal(err)
		}
	}

	// Mesmo instante em todos os lançamentos: a ordem vem do Seq, não do relógio.
	var balances []int64
	var before int64
	for page := 0; ; page++ {
		lines, err := s.Statement(ctx, a.ID(), before, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) == 0 {
			break
		}
		if page > 3 {
			t.Fatal("paginação não termina")
		}
		for _, l := range lines {
			if before != 0 && l.Seq >= before {
				t.Fatalf("Seq %d não é menor que o cursor %d", l.Seq, before)
			}
			balances = append(balances, l.Posting.BalanceAfter.Cents())
			before = l.Seq
		}
	}
	want := []int64{1500, 1000, 600, 300, 100}
	if len(balances) != len(want) {
		t.Fatalf("saldos = %v, want %v", balances, want)
	}
	for i := range want {
		if balances[i] != want[i] {
			t.Fatalf("saldos = %v, want %v", balances, want)
		}
	}
}

func TestWithTxRollsBack(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	a := newCustomer(t)
	create(t, s, a)
	boom := errors.New("falha depois de lançar")

	err := s.WithTx(ctx, func(tx account.Tx) error {
		acc, err := tx.LockAccount(ctx, a.ID())
		if err != nil {
			return err
		}
		e, err := acc.Credit(movement(t, 100, openedAt), account.CreditOpening)
		if err != nil {
			return err
		}
		if err := tx.AppendEntry(ctx, e); err != nil {
			return err
		}
		if err := tx.SaveBalance(ctx, acc); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want o erro de fn", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("o panic de fn deveria seguir adiante")
			}
		}()
		_ = s.WithTx(ctx, func(tx account.Tx) error {
			_ = post(ctx, s, a.ID(), credit(t, movement(t, 1, openedAt))) // outra transação, confirmada
			acc, _ := tx.LockAccount(ctx, a.ID())
			e, _ := acc.Credit(movement(t, 100, openedAt), account.CreditOpening)
			_ = tx.AppendEntry(ctx, e)
			panic("bug no caso de uso")
		})
	}()

	got, _ := s.Account(ctx, a.ID())
	lines, _ := s.Statement(ctx, a.ID(), 0, 10)
	if got.Balance().Cents() != 1 || len(lines) != 1 {
		t.Fatalf("saldo %d com %d linhas; want só o crédito de 1 confirmado fora das transações desfeitas",
			got.Balance().Cents(), len(lines))
	}
}

func TestLockTimeoutIsRetryable(t *testing.T) {
	_, pool := pgtest.Migrated(t, 50*time.Millisecond)
	s := postgres.NewStore(pool)
	ctx := t.Context()
	a := newCustomer(t)
	create(t, s, a)

	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- s.WithTx(ctx, func(tx account.Tx) error {
			if _, err := tx.LockAccount(ctx, a.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	start := time.Now()
	err := s.WithTx(ctx, func(tx account.Tx) error {
		_, err := tx.LockAccount(ctx, a.ID())
		return err
	})
	waited := time.Since(start)
	close(release)
	if herr := <-holderDone; herr != nil {
		t.Fatalf("transação que segurava o lock: %v", herr)
	}

	var r interface{ Retryable() bool }
	if !errors.As(err, &r) || !r.Retryable() {
		t.Fatalf("err = %v, want erro com Retryable() true", err)
	}
	if waited < 50*time.Millisecond || waited > 2*time.Second {
		t.Errorf("esperou %s pelo lock, want por volta do lock_timeout de 50ms", waited)
	}
}

func TestIdempotencyKeyLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	hash := make([]byte, 32)
	hash[0] = 7

	err := s.WithTx(ctx, func(tx account.Tx) error {
		claimed, err := tx.ClaimKey(ctx, "debit", "k1", hash)
		if err != nil || !claimed {
			t.Fatalf("primeira reserva = %v, %v; want true", claimed, err)
		}
		rec, err := tx.KeyRecord(ctx, "debit", "k1")
		if err != nil || rec.Completed {
			t.Fatalf("registro pendente = %+v, %v", rec, err)
		}
		return tx.CompleteKey(ctx, "debit", "k1", 9, []byte("recusa"))
	})
	if err != nil {
		t.Fatal(err)
	}

	err = s.WithTx(ctx, func(tx account.Tx) error {
		claimed, err := tx.ClaimKey(ctx, "debit", "k1", hash)
		if err != nil || claimed {
			t.Fatalf("segunda reserva = %v, %v; want false (a chave já existe)", claimed, err)
		}
		rec, err := tx.KeyRecord(ctx, "debit", "k1")
		if err != nil {
			return err
		}
		if !rec.Completed || rec.StatusCode != 9 || string(rec.Body) != "recusa" || rec.RequestHash[0] != 7 {
			t.Fatalf("registro = %+v", rec)
		}
		// A mesma chave em outra operação é outra chave.
		claimed, err = tx.ClaimKey(ctx, "credit", "k1", hash)
		if err != nil || !claimed {
			t.Fatalf("mesma chave em outro escopo = %v, %v; want true", claimed, err)
		}
		if err := tx.CompleteKey(ctx, "debit", "k1", 0, []byte("x")); err == nil {
			t.Fatal("completar uma chave já completa deveria falhar")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Duas requisições com a mesma chave ao mesmo tempo: a segunda espera a primeira no índice
// único e então enxerga o resultado dela, ou herda a reserva se a primeira desistir.
func TestConcurrentClaimWaitsForTheFirst(t *testing.T) {
	tests := []struct {
		name        string
		firstCommit bool
		wantClaimed bool
	}{
		{name: "primeira confirma: a segunda vê a chave", firstCommit: true, wantClaimed: false},
		{name: "primeira desfaz: a segunda fica com a chave", firstCommit: false, wantClaimed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			ctx := t.Context()
			hash := make([]byte, 32)

			claimed := make(chan struct{})
			finish := make(chan struct{})
			firstDone := make(chan error, 1)
			go func() {
				firstDone <- s.WithTx(ctx, func(tx account.Tx) error {
					if _, err := tx.ClaimKey(ctx, "debit", "k", hash); err != nil {
						return err
					}
					close(claimed)
					<-finish
					if !tt.firstCommit {
						return errors.New("desiste")
					}
					return tx.CompleteKey(ctx, "debit", "k", 0, []byte("ok"))
				})
			}()
			<-claimed

			second := make(chan bool, 1)
			secondErr := make(chan error, 1)
			go func() {
				secondErr <- s.WithTx(ctx, func(tx account.Tx) error {
					c, err := tx.ClaimKey(ctx, "debit", "k", hash)
					second <- c
					return err
				})
			}()

			select {
			case <-second:
				t.Fatal("a segunda reserva não esperou a primeira transação terminar")
			case <-time.After(150 * time.Millisecond):
			}
			close(finish)
			if err := <-firstDone; (err == nil) != tt.firstCommit {
				t.Fatalf("primeira transação: err = %v, want commit = %v", err, tt.firstCommit)
			}

			if got := <-second; got != tt.wantClaimed {
				t.Fatalf("segunda reserva = %v, want %v", got, tt.wantClaimed)
			}
			if err := <-secondErr; err != nil {
				t.Fatal(err)
			}
		})
	}
}
