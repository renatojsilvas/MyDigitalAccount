package account_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/platform/postgres/pgtest"
	"conta.local/ledger/internal/postgres"
)

// fakeCodec faz o papel da borda gRPC (passo 5): grava resultados em JSON e recusas como
// um código e uma mensagem. A repetição de uma recusa volta como replayedRejection.
type fakeCodec struct{}

type postingJSON struct {
	EntryID, AccountID string
	Direction          int
	Amount, Balance    int64
	Description        string
	PostedAt           time.Time
}

type accountJSON struct {
	ID, Holder, Document string
	CreatedAt            time.Time
}

type replayedRejection struct {
	Code int32
	Msg  string
}

func (r replayedRejection) Error() string { return fmt.Sprintf("replay %d: %s", r.Code, r.Msg) }

func (fakeCodec) EncodePosting(p account.Posting) ([]byte, error) {
	return json.Marshal(postingJSON{
		EntryID: p.EntryID.String(), AccountID: p.AccountID.String(), Direction: int(p.Direction),
		Amount: p.Amount.Cents(), Balance: p.BalanceAfter.Cents(), Description: p.Description.String(), PostedAt: p.PostedAt,
	})
}

func (fakeCodec) DecodePosting(b []byte) (account.Posting, error) {
	var j postingJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return account.Posting{}, err
	}
	entry, err := account.ParseID(j.EntryID)
	if err != nil {
		return account.Posting{}, err
	}
	id, err := account.ParseID(j.AccountID)
	if err != nil {
		return account.Posting{}, err
	}
	desc, err := account.NewDescription(j.Description)
	if err != nil {
		return account.Posting{}, err
	}
	return account.Posting{
		EntryID: account.EntryID(entry), AccountID: id, Direction: account.Direction(j.Direction),
		Amount: account.MoneyFromCents(j.Amount), BalanceAfter: account.MoneyFromCents(j.Balance),
		Description: desc, PostedAt: j.PostedAt,
	}, nil
}

func (fakeCodec) EncodeAccount(a account.Account) ([]byte, error) {
	s := a.Snapshot()
	return json.Marshal(accountJSON{ID: s.ID.String(), Holder: s.Holder.String(), Document: s.Document.Digits(), CreatedAt: s.CreatedAt})
}

func (fakeCodec) DecodeAccount(b []byte) (account.Account, error) {
	var j accountJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return account.Account{}, err
	}
	id, err := account.ParseID(j.ID)
	if err != nil {
		return account.Account{}, err
	}
	holder, err := account.NewHolderName(j.Holder)
	if err != nil {
		return account.Account{}, err
	}
	doc, err := account.ParseCPF(j.Document)
	if err != nil {
		return account.Account{}, err
	}
	return account.Open(id, holder, doc, j.CreatedAt), nil
}

func (fakeCodec) EncodeRejection(err error) (int32, []byte, error) {
	switch {
	case errors.Is(err, account.ErrAccountNotFound):
		return 5, []byte(err.Error()), nil
	case errors.Is(err, account.ErrAccountAlreadyExists):
		return 6, []byte(err.Error()), nil
	case errors.Is(err, account.ErrInsufficientFunds):
		return 9, []byte(err.Error()), nil
	}
	return 0, nil, fmt.Errorf("não é recusa de negócio: %w", err)
}

func (fakeCodec) DecodeRejection(code int32, body []byte) error {
	return replayedRejection{Code: code, Msg: string(body)}
}

type fixture struct {
	svc   *account.Service
	store *postgres.Store
}

func newFixture(t *testing.T, lockTimeout time.Duration, opts ...account.Option) fixture {
	t.Helper()
	_, pool := pgtest.Migrated(t, lockTimeout)
	store := postgres.NewStore(pool)
	return fixture{svc: account.NewService(store, fakeCodec{}, opts...), store: store}
}

var keySeq atomic.Int64

func newKey(t *testing.T) account.IdempotencyKey {
	t.Helper()
	k, err := account.NewIdempotencyKey(fmt.Sprintf("key-%d-%d", time.Now().UnixNano(), keySeq.Add(1)))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustAmount(t *testing.T, cents int64) account.Money {
	t.Helper()
	m, err := account.NewAmount(cents)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// open abre a conta de teste, com o CPF 111.444.777-35.
func (f fixture) open(t *testing.T) account.Account {
	t.Helper()
	holder, _ := account.NewHolderName("Cliente Teste")
	doc, err := account.ParseCPF("11144477735")
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.svc.OpenAccount(t.Context(), account.OpenAccountCommand{Key: newKey(t), Holder: holder, Document: doc})
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	return a
}

func (f fixture) fund(t *testing.T, id account.ID, cents int64) {
	t.Helper()
	if cents == 0 {
		return
	}
	_, err := f.svc.Credit(t.Context(), account.CreditCommand{
		Key: newKey(t), AccountID: id, Amount: mustAmount(t, cents), Reason: account.CreditOpening,
	})
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
}

func debitCmd(t *testing.T, key account.IdempotencyKey, id account.ID, cents int64) account.DebitCommand {
	t.Helper()
	return account.DebitCommand{Key: key, AccountID: id, Amount: mustAmount(t, cents), Reason: account.DebitExternal}
}

func (f fixture) balance(t *testing.T, id account.ID) int64 {
	t.Helper()
	b, err := f.svc.Balance(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Cents()
}

// allLines lê o extrato inteiro, da linha mais antiga para a mais recente.
func (f fixture) allLines(t *testing.T, id account.ID) []account.StatementLine {
	t.Helper()
	var newestFirst []account.StatementLine
	var cursor int64
	for {
		page, err := f.svc.Statement(t.Context(), id, cursor, 100)
		if err != nil {
			t.Fatal(err)
		}
		newestFirst = append(newestFirst, page.Lines...)
		if page.Next == 0 {
			break
		}
		cursor = page.Next
	}
	lines := make([]account.StatementLine, len(newestFirst))
	for i, l := range newestFirst {
		lines[len(lines)-1-i] = l
	}
	return lines
}

// assertLedgerConsistent confere as invariantes da conta depois de qualquer carga: saldo
// gravado igual à soma das pernas e extrato encadeado (cada saldo posterior é o anterior
// mais ou menos o valor da perna), sem nenhum saldo negativo no caminho.
func (f fixture) assertLedgerConsistent(t *testing.T, id account.ID) {
	t.Helper()
	balance := f.balance(t, id)
	net, err := f.store.NetPostings(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if net.Cents() != balance {
		t.Fatalf("saldo gravado %d, soma das pernas %d", balance, net.Cents())
	}
	var running int64
	for _, l := range f.allLines(t, id) {
		p := l.Posting
		if p.Direction == account.DirectionCredit {
			running += p.Amount.Cents()
		} else {
			running -= p.Amount.Cents()
		}
		if p.BalanceAfter.Cents() != running {
			t.Fatalf("perna %d: saldo posterior %d, esperado %d pelo encadeamento", l.Seq, p.BalanceAfter.Cents(), running)
		}
		if running < 0 {
			t.Fatalf("perna %d deixou o saldo negativo: %d", l.Seq, running)
		}
	}
	if running != balance {
		t.Fatalf("extrato termina em %d, saldo gravado %d", running, balance)
	}
}

// Critério de pronto da Fase 1: débitos concorrentes na mesma conta nunca deixam o saldo
// negativo. Todas as goroutines partem juntas, cada uma com a sua chave; o lock pessimista
// serializa os débitos, e exatamente os que cabem no saldo passam.
func TestConcurrentDebitsNeverOverdraw(t *testing.T) {
	tests := []struct {
		name    string
		initial int64
		debits  int
		amount  int64
		wantOK  int
	}{
		{name: "dois débitos de 80 numa conta com 100", initial: 100, debits: 2, amount: 80, wantOK: 1},
		{name: "50 débitos de 300 numa conta com 10.000", initial: 10_000, debits: 50, amount: 300, wantOK: 33},
		{name: "10 débitos que zeram a conta exatamente", initial: 10_000, debits: 10, amount: 1_000, wantOK: 10},
		{name: "20 débitos numa conta vazia", initial: 0, debits: 20, amount: 1, wantOK: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, 5*time.Second)
			a := f.open(t)
			f.fund(t, a.ID(), tt.initial)

			cmds := make([]account.DebitCommand, tt.debits)
			for i := range cmds {
				cmds[i] = debitCmd(t, newKey(t), a.ID(), tt.amount)
			}
			errs := make([]error, tt.debits)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := range cmds {
				wg.Go(func() {
					<-start
					_, errs[i] = f.svc.Debit(context.Background(), cmds[i])
				})
			}
			close(start)
			wg.Wait()

			var ok, refused int
			for i, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, account.ErrInsufficientFunds):
					refused++
				default:
					t.Fatalf("débito %d: erro inesperado: %v", i, err)
				}
			}
			if ok != tt.wantOK || refused != tt.debits-tt.wantOK {
				t.Fatalf("aceitos %d, recusados %d; want %d e %d", ok, refused, tt.wantOK, tt.debits-tt.wantOK)
			}
			want := tt.initial - int64(ok)*tt.amount
			if got := f.balance(t, a.ID()); got != want || got < 0 {
				t.Fatalf("saldo final %d, want %d", got, want)
			}
			f.assertLedgerConsistent(t, a.ID())
		})
	}
}

// A mesma chave enviada várias vezes ao mesmo tempo (retries agressivos): um único
// lançamento, e todas as chamadas recebem a mesma resposta.
func TestConcurrentRetriesWithSameKeyPostOnce(t *testing.T) {
	f := newFixture(t, 5*time.Second)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	cmd := debitCmd(t, newKey(t), a.ID(), 100)

	const n = 10
	results := make([]account.Posting, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			results[i], errs[i] = f.svc.Debit(context.Background(), cmd)
		})
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("chamada %d: %v", i, errs[i])
		}
		if results[i].EntryID != results[0].EntryID || !results[i].PostedAt.Equal(results[0].PostedAt) ||
			results[i].BalanceAfter != results[0].BalanceAfter {
			t.Fatalf("chamada %d recebeu %+v, a primeira %+v", i, results[i], results[0])
		}
	}
	if got := f.balance(t, a.ID()); got != 900 {
		t.Fatalf("saldo %d, want 900 (um único débito)", got)
	}
	f.assertLedgerConsistent(t, a.ID())
}

func TestDebitReplayReturnsTheOriginalResult(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	cmd := debitCmd(t, newKey(t), a.ID(), 300)

	first, err := f.svc.Debit(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	f.fund(t, a.ID(), 5_000) // o saldo muda entre a chamada e a repetição
	again, err := f.svc.Debit(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("repetição = %+v, original = %+v", again, first)
	}
	if got := f.balance(t, a.ID()); got != 5_700 {
		t.Fatalf("saldo %d, want 5700 (a repetição não debita de novo)", got)
	}
}

// ADR-0004, seção 3: uma recusa de negócio é gravada e repetida pela mesma chave, mesmo que
// o saldo tenha passado a cobrir o débito. Para tentar de novo, o cliente usa outra chave.
func TestRejectionIsReplayedEvenAfterBalanceChanges(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 100)
	cmd := debitCmd(t, newKey(t), a.ID(), 500)

	if _, err := f.svc.Debit(t.Context(), cmd); !errors.Is(err, account.ErrInsufficientFunds) {
		t.Fatalf("primeira chamada: err = %v, want ErrInsufficientFunds", err)
	}
	f.fund(t, a.ID(), 1_000)

	_, err := f.svc.Debit(t.Context(), cmd)
	var replayed replayedRejection
	if !errors.As(err, &replayed) || replayed.Code != 9 || replayed.Msg != account.ErrInsufficientFunds.Error() {
		t.Fatalf("repetição: err = %v, want a mesma recusa gravada", err)
	}
	if _, err := f.svc.Debit(t.Context(), debitCmd(t, newKey(t), a.ID(), 500)); err != nil {
		t.Fatalf("com chave nova o débito deve passar: %v", err)
	}
	if got := f.balance(t, a.ID()); got != 600 {
		t.Fatalf("saldo %d, want 600", got)
	}
}

func TestIdempotencyKeyReusedWithDifferentRequest(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	key := newKey(t)

	if _, err := f.svc.Debit(t.Context(), debitCmd(t, key, a.ID(), 100)); err != nil {
		t.Fatal(err)
	}
	other := debitCmd(t, key, a.ID(), 101)
	if _, err := f.svc.Debit(t.Context(), other); !errors.Is(err, account.ErrIdempotencyKeyReused) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
	}
	desc, _ := account.NewDescription("outra descrição")
	other = debitCmd(t, key, a.ID(), 100)
	other.Description = desc
	if _, err := f.svc.Debit(t.Context(), other); !errors.Is(err, account.ErrIdempotencyKeyReused) {
		t.Fatalf("descrição diferente: err = %v, want ErrIdempotencyKeyReused", err)
	}
	// A mesma chave num crédito é outra chave (escopo por operação).
	_, err := f.svc.Credit(t.Context(), account.CreditCommand{
		Key: key, AccountID: a.ID(), Amount: mustAmount(t, 100), Reason: account.CreditExternal,
	})
	if err != nil {
		t.Fatalf("crédito com a mesma chave: %v", err)
	}
	if got := f.balance(t, a.ID()); got != 1_000 {
		t.Fatalf("saldo %d, want 1000", got)
	}
}

// Erro de infraestrutura (aqui, lock timeout) desfaz a reserva da chave: a nova tentativa
// com a mesma chave executa de verdade, em vez de repetir um erro que não era do negócio.
func TestInfrastructureErrorIsNotRecorded(t *testing.T) {
	f := newFixture(t, 50*time.Millisecond)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	cmd := debitCmd(t, newKey(t), a.ID(), 100)

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- f.store.WithTx(context.Background(), func(tx account.Tx) error {
			if _, err := tx.LockAccount(context.Background(), a.ID()); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	_, err := f.svc.Debit(t.Context(), cmd)
	close(release)
	if herr := <-done; herr != nil {
		t.Fatal(herr)
	}
	var r interface{ Retryable() bool }
	if !errors.As(err, &r) {
		t.Fatalf("com a conta travada: err = %v, want erro repetível", err)
	}

	if _, err := f.svc.Debit(t.Context(), cmd); err != nil {
		t.Fatalf("nova tentativa com a mesma chave: %v", err)
	}
	if got := f.balance(t, a.ID()); got != 900 {
		t.Fatalf("saldo %d, want 900", got)
	}
}

func TestDebitUnknownAccountIsRecordedRejection(t *testing.T) {
	f := newFixture(t, 0)
	missing, _ := account.NewID()
	cmd := debitCmd(t, newKey(t), missing, 100)

	if _, err := f.svc.Debit(t.Context(), cmd); !errors.Is(err, account.ErrAccountNotFound) {
		t.Fatalf("err = %v, want ErrAccountNotFound", err)
	}
	var replayed replayedRejection
	if _, err := f.svc.Debit(t.Context(), cmd); !errors.As(err, &replayed) || replayed.Code != 5 {
		t.Fatalf("repetição: err = %v, want a recusa gravada", err)
	}
}

func TestOpenAccount(t *testing.T) {
	f := newFixture(t, 0)
	holder, _ := account.NewHolderName("Ana Souza")
	doc, _ := account.ParseCPF("52998224725")
	cmd := account.OpenAccountCommand{Key: newKey(t), Holder: holder, Document: doc}

	first, err := f.svc.OpenAccount(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.OpenAccount(t.Context(), cmd)
	if err != nil || again.ID() != first.ID() || !again.Snapshot().CreatedAt.Equal(first.Snapshot().CreatedAt) {
		t.Fatalf("repetição = %v, %v; want a mesma conta %s", again.ID(), err, first.ID())
	}

	dup := account.OpenAccountCommand{Key: newKey(t), Holder: holder, Document: doc}
	if _, err := f.svc.OpenAccount(t.Context(), dup); !errors.Is(err, account.ErrAccountAlreadyExists) {
		t.Fatalf("mesmo CPF com outra chave: err = %v, want ErrAccountAlreadyExists", err)
	}
	var replayed replayedRejection
	if _, err := f.svc.OpenAccount(t.Context(), dup); !errors.As(err, &replayed) || replayed.Code != 6 {
		t.Fatalf("repetição da recusa: err = %v", err)
	}

	otherName, _ := account.NewHolderName("Outro Nome")
	reuse := account.OpenAccountCommand{Key: cmd.Key, Holder: otherName, Document: doc}
	if _, err := f.svc.OpenAccount(t.Context(), reuse); !errors.Is(err, account.ErrIdempotencyKeyReused) {
		t.Fatalf("chave reutilizada: err = %v, want ErrIdempotencyKeyReused", err)
	}

	got, err := f.svc.Account(t.Context(), first.ID())
	if err != nil || got.Snapshot().Document.Digits() != "52998224725" || got.Balance().Cents() != 0 {
		t.Fatalf("Account = %+v, %v", got.Snapshot(), err)
	}
}

// O relógio pode ter nanossegundos; o Postgres guarda microssegundos. A resposta da primeira
// chamada precisa bater com o extrato e com a repetição.
func TestPostedAtMatchesStoredPrecision(t *testing.T) {
	clock := func() time.Time {
		return time.Date(2026, 11, 11, 12, 0, 0, 123_456_789, time.FixedZone("BRT", -3*3600))
	}
	f := newFixture(t, 0, account.WithClock(clock))
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)

	p, err := f.svc.Debit(t.Context(), debitCmd(t, newKey(t), a.ID(), 1))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 11, 15, 0, 0, 123_456_000, time.UTC)
	if !p.PostedAt.Equal(want) || p.PostedAt.Location() != time.UTC {
		t.Fatalf("PostedAt = %s, want %s", p.PostedAt, want)
	}
	lines := f.allLines(t, a.ID())
	if last := lines[len(lines)-1].Posting; !last.PostedAt.Equal(p.PostedAt) {
		t.Fatalf("extrato %s, resposta %s", last.PostedAt, p.PostedAt)
	}
}

func TestStatementPaging(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	for range 5 {
		f.fund(t, a.ID(), 100)
	}

	page, err := f.svc.Statement(t.Context(), a.ID(), 0, 2)
	if err != nil || len(page.Lines) != 2 || page.Next == 0 {
		t.Fatalf("primeira página = %d linhas, next %d, %v", len(page.Lines), page.Next, err)
	}
	if page.Lines[0].Posting.BalanceAfter.Cents() != 500 {
		t.Fatalf("a primeira linha deve ser a mais recente (saldo 500), veio %d", page.Lines[0].Posting.BalanceAfter.Cents())
	}
	last, err := f.svc.Statement(t.Context(), a.ID(), 0, 5)
	if err != nil || len(last.Lines) != 5 || last.Next != 0 {
		t.Fatalf("página exata = %d linhas, next %d, %v; want 5 e sem próxima", len(last.Lines), last.Next, err)
	}
	def, err := f.svc.Statement(t.Context(), a.ID(), 0, 0)
	if err != nil || len(def.Lines) != 5 {
		t.Fatalf("tamanho padrão = %d linhas, %v", len(def.Lines), err)
	}

	invalid := []struct {
		name     string
		cursor   int64
		pageSize int
		field    string
	}{
		{"página grande demais", 0, 101, "page_size"},
		{"página negativa", 0, -1, "page_size"},
		{"cursor negativo", -1, 10, "page_token"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			var verr *account.ValidationError
			if _, err := f.svc.Statement(t.Context(), a.ID(), tt.cursor, tt.pageSize); !errors.As(err, &verr) || verr.Field != tt.field {
				t.Fatalf("err = %v, want ValidationError em %s", err, tt.field)
			}
		})
	}

	missing, _ := account.NewID()
	if _, err := f.svc.Statement(t.Context(), missing, 0, 10); !errors.Is(err, account.ErrAccountNotFound) {
		t.Fatalf("conta inexistente: err = %v, want ErrAccountNotFound", err)
	}
	if _, err := f.svc.Balance(t.Context(), missing); !errors.Is(err, account.ErrAccountNotFound) {
		t.Fatalf("saldo de conta inexistente: err = %v, want ErrAccountNotFound", err)
	}
}
