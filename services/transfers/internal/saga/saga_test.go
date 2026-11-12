package saga_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/postgres"
	"conta.local/transfers/internal/postgres/pgtest"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

const (
	sourceBalance = 100_000
	sourceCPF     = "12345678909"
	destCPF       = "98765432100"
	limit         = 200_000
)

type world struct {
	store     *postgres.Store
	svc       *pix.Service
	runner    *saga.Runner
	antifraud *fakeAntifraud
	ledger    *fakeLedger
	spi       *fakeSPI
	rec       *recorder
	source    uuid.UUID
	dest      uuid.UUID
}

func newWorld(t *testing.T, script ...attempt) *world {
	t.Helper()
	return newWorldWith(t, time.Hour, script...)
}

// newWorldWith permite escolher o prazo máximo de confirmação.
func newWorldWith(t *testing.T, deadline time.Duration, script ...attempt) *world {
	t.Helper()
	_, pool := pgtest.Migrated(t)
	cal, err := pix.NewCalendar()
	if err != nil {
		t.Fatal(err)
	}
	if len(script) == 0 {
		script = []attempt{spiSettled}
	}
	w := &world{
		store: postgres.NewStore(pool), antifraud: &fakeAntifraud{approve: true}, ledger: newLedger(),
		spi: &fakeSPI{script: script}, rec: newRecorder(),
	}
	w.source = w.ledger.open("52998224725", sourceBalance)
	w.dest = w.ledger.open(destCPF, 0)
	w.svc = pix.NewService(w.store, cal, []byte("segredo-de-teste-com-32-bytes-ok"), limit)
	w.runner = w.newRunner(deadline)
	return w
}

// newRunner monta um runner sobre o mesmo banco e as mesmas dependências: uma segunda
// instância, ou o processo depois de um reinício.
func (w *world) newRunner(deadline time.Duration) *saga.Runner {
	return saga.NewRunner(saga.Config{
		Owner: "teste", LeaseTTL: 30 * time.Second, Workers: 1, PollInterval: time.Hour, RetryDelay: 0,
		DailyLimitCents: limit, FanOutTimeout: 2 * time.Second, SettleAttempts: 3,
		SettleBackoff:   resilience.Policy{Base: time.Millisecond, Max: 2 * time.Millisecond},
		ConfirmAfter:    0,
		ConfirmInterval: resilience.Policy{Base: time.Millisecond, Max: 2 * time.Millisecond},
		ConfirmDeadline: deadline, ConfirmWorkers: 1,
	}, saga.Deps{
		Store: w.store, Antifraud: w.antifraud, Ledger: w.ledger, SPI: w.spi, Tracing: noTracing{}, Observer: w.rec,
		Logger: slog.New(slog.DiscardHandler),
	})
}

func (w *world) pix(t *testing.T, key string, cents int64, destKey string) pix.Transfer {
	t.Helper()
	cmd, err := pix.NewStartCommand(key, w.source.String(), pix.KeyCPF, destKey, cents, "aluguel")
	if err != nil {
		t.Fatal(err)
	}
	tr, err := w.svc.Start(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// drain roda o runner até não haver mais nada vencido.
func (w *world) drain(t *testing.T) {
	t.Helper()
	for range 20 {
		n, err := w.runner.RunOnce(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("o runner não parou de encontrar trabalho")
}

func (w *world) get(t *testing.T, id uuid.UUID) (pix.Transfer, []pix.Change) {
	t.Helper()
	tr, h, err := w.store.Get(t.Context(), w.source, id)
	if err != nil {
		t.Fatal(err)
	}
	return tr, h
}

func (w *world) used(t *testing.T, day pix.Day) int64 {
	t.Helper()
	u, err := w.store.UsedOn(t.Context(), w.source, day)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func states(h []pix.Change) []pix.State {
	out := make([]pix.State, len(h))
	for i, c := range h {
		out[i] = c.To
	}
	return out
}

func equalStates(a, b []pix.State) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSuccess(t *testing.T) {
	w := newWorld(t)
	tr := w.pix(t, "k", 30_000, destCPF)
	w.drain(t)

	got, h := w.get(t, tr.ID)
	want := []pix.State{pix.StateAccepted, pix.StateValidated, pix.StateDebited, pix.StateSettling, pix.StateSettled, pix.StateCompleted}
	if got.State != pix.StateCompleted || got.State.Status() != pix.StatusCompleted || !equalStates(states(h), want) {
		t.Fatalf("estado %v, histórico %v", got.State, states(h))
	}
	if w.ledger.balance[w.source] != 70_000 || w.ledger.balance[w.dest] != 30_000 || w.ledger.debits != 1 || w.ledger.credits != 1 {
		t.Fatalf("ledger: origem %d, destino %d, débitos %d, créditos %d",
			w.ledger.balance[w.source], w.ledger.balance[w.dest], w.ledger.debits, w.ledger.credits)
	}
	if got.DestinationAccountID != w.dest || got.DebitEntryID == (uuid.UUID{}) || !got.SettlementMaybeSent {
		t.Fatalf("transferência = %+v", got)
	}
	if u := w.used(t, tr.LimitDay); u != 30_000 {
		t.Fatalf("limite usado %d, want 30000 (Pix concluído consome o limite)", u)
	}
}

// Recusas antes do débito: nada sai da conta, e a reserva do limite (se houve) volta.
func TestRejectionsBeforeDebit(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(w *world)
		cents   int64
		destKey string
		want    pix.Reason
	}{
		{"antifraude recusa", func(w *world) { w.antifraud.approve = false }, 1_000, destCPF, pix.ReasonAntifraudDenied},
		{"antifraude fora do ar", func(w *world) { w.antifraud.err = saga.ErrUnavailable }, 1_000, destCPF, pix.ReasonDependencyUnavailable},
		{"acima do limite diário", func(*world) {}, limit + 1, destCPF, pix.ReasonLimitExceeded},
		{"chave sem conta", func(*world) {}, 1_000, "11144477735", pix.ReasonDestinationInvalid},
		{"para a própria conta", func(*world) {}, 1_000, "52998224725", pix.ReasonSameAccount},
		{"ledger fora do ar na consulta do destino", func(w *world) { w.ledger.lookupErr = saga.ErrUnavailable }, 1_000, destCPF, pix.ReasonDependencyUnavailable},
		{"saldo insuficiente", func(*world) {}, 150_000, destCPF, pix.ReasonInsufficientFunds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			tt.setup(w)
			tr := w.pix(t, "k", tt.cents, tt.destKey)
			w.drain(t)
			got, _ := w.get(t, tr.ID)
			if got.State != pix.StateRejected || got.Reason != tt.want || got.State.Status() != pix.StatusRejected {
				t.Fatalf("estado %v (%v), want rejected (%v)", got.State, got.Reason, tt.want)
			}
			if w.ledger.balance[w.source] != 100_000 || w.ledger.reversals != 0 {
				t.Fatalf("saldo %d, estornos %d: nada deveria ter saído", w.ledger.balance[w.source], w.ledger.reversals)
			}
			if u := w.used(t, tr.LimitDay); u != 0 || got.LimitReserved {
				t.Fatalf("limite usado %d (reservado=%v), want devolvido", u, got.LimitReserved)
			}
		})
	}
}

// Critério de pronto: toda recusa explícita na liquidação termina em estorno.
func TestSettlementRejectedEndsInReversal(t *testing.T) {
	w := newWorld(t, spiRejected)
	tr := w.pix(t, "k", 30_000, destCPF)
	w.drain(t)
	got, h := w.get(t, tr.ID)
	want := []pix.State{pix.StateAccepted, pix.StateValidated, pix.StateDebited, pix.StateSettling, pix.StateReversing, pix.StateReversed}
	if got.State != pix.StateReversed || got.Reason != pix.ReasonSettlementRejected || !equalStates(states(h), want) {
		t.Fatalf("estado %v (%v), histórico %v", got.State, got.Reason, states(h))
	}
	if w.ledger.reversals != 1 || w.ledger.credits != 0 || w.spi.calls != 1 {
		t.Fatalf("estornos %d, créditos %d, chamadas ao SPI %d", w.ledger.reversals, w.ledger.credits, w.spi.calls)
	}
	if u := w.used(t, tr.LimitDay); u != 0 || w.rec.compensations["reversal"] != 1 || w.rec.compensations["limit_release"] != 1 {
		t.Fatalf("limite %d, compensações %v", u, w.rec.compensations)
	}
}

// Critério de pronto: timeout na liquidação nunca estorna nem conclui sem confirmação.
func TestSettlementOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		script       []attempt
		wantState    pix.State
		wantReason   pix.Reason
		wantMaybe    bool
		wantSPICalls int
	}{
		{"timeout e depois liquidado", []attempt{spiTimeout, spiSettled}, pix.StateCompleted, 0, true, 2},
		{"timeout e depois recusa explícita", []attempt{spiTimeout, spiRejected}, pix.StateReversed, pix.ReasonSettlementRejected, true, 2},
		{"só timeouts: pendente, com o débito", []attempt{spiTimeout, spiTimeout, spiTimeout}, pix.StatePendingConfirmation, 0, true, 3},
		// Ajuste do autor ao ADR-0007: uma tentativa que pode ter saído vale para todas.
		{"timeout e depois circuito aberto: pendente, nunca estorno",
			[]attempt{spiTimeout, spiNotSent, spiNotSent}, pix.StatePendingConfirmation, 0, true, 3},
		{"circuito aberto em todas: não enviado, com prova", []attempt{spiNotSent}, pix.StateReversed, pix.ReasonSettlementNotSent, false, 3},
		{"circuito aberto e depois liquidado", []attempt{spiNotSent, spiSettled}, pix.StateCompleted, 0, true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.script...)
			tr := w.pix(t, "k", 30_000, destCPF)
			w.drain(t)
			got, _ := w.get(t, tr.ID)
			if got.State != tt.wantState || got.Reason != tt.wantReason || got.SettlementMaybeSent != tt.wantMaybe {
				t.Fatalf("estado %v (%v), maybe_sent=%v; want %v (%v), %v",
					got.State, got.Reason, got.SettlementMaybeSent, tt.wantState, tt.wantReason, tt.wantMaybe)
			}
			if w.spi.calls != tt.wantSPICalls {
				t.Fatalf("chamadas ao SPI %d, want %d", w.spi.calls, tt.wantSPICalls)
			}
			if tt.wantState == pix.StatePendingConfirmation {
				// Débito mantido, sem estorno nem crédito; o cliente vê "em processamento".
				if w.ledger.reversals != 0 || w.ledger.credits != 0 || w.ledger.balance[w.source] != 70_000 ||
					got.State.Status() != pix.StatusProcessing {
					t.Fatalf("pendente com estornos %d, créditos %d, saldo %d, status %v",
						w.ledger.reversals, w.ledger.credits, w.ledger.balance[w.source], got.State.Status())
				}
				// O runner não volta a mexer num pendente: é do worker de confirmação (passo 8).
				calls := w.spi.calls
				w.drain(t)
				if w.spi.calls != calls {
					t.Fatal("o runner tentou liquidar de novo um Pix pendente")
				}
			}
		})
	}
}

// Ledger fora do ar no débito: o Pix espera no mesmo estado (o débito pode ter acontecido),
// e a mesma chave resolve depois, sem débito duplicado.
func TestLedgerUnavailableOnDebitRetriesWithSameKey(t *testing.T) {
	w := newWorld(t)
	w.ledger.failDebit = 2
	tr := w.pix(t, "k", 30_000, destCPF)

	if _, err := w.runner.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, _ := w.get(t, tr.ID); got.State != pix.StateValidated {
		t.Fatalf("depois da falha: %v, want validated (espera, nunca recusa)", got.State)
	}
	w.drain(t)
	if got, _ := w.get(t, tr.ID); got.State != pix.StateCompleted || w.ledger.debits != 1 {
		t.Fatalf("estado %v, débitos %d; want completed com um débito", got.State, w.ledger.debits)
	}
}

// Queda no meio da saga: o ledger aplicou o débito, mas a resposta se perdeu (o processo caiu
// antes de gravar DEBITED). A retomada repete com a mesma chave e recebe o mesmo lançamento.
func TestCrashAfterDebitNeverDebitsTwice(t *testing.T) {
	w := newWorld(t)
	w.ledger.failDebit, w.ledger.applyThenFail = 1, true
	tr := w.pix(t, "k", 30_000, destCPF)
	w.drain(t)
	got, _ := w.get(t, tr.ID)
	if got.State != pix.StateCompleted || w.ledger.debits != 1 || w.ledger.balance[w.source] != 70_000 {
		t.Fatalf("estado %v, débitos %d, saldo %d; want um único débito", got.State, w.ledger.debits, w.ledger.balance[w.source])
	}
}

// Critério de pronto: repetir a mesma requisição não gera débito duplicado.
func TestRepeatedRequestDebitsOnce(t *testing.T) {
	w := newWorld(t)
	first := w.pix(t, "mesma-chave", 30_000, destCPF)
	w.drain(t)
	again := w.pix(t, "mesma-chave", 30_000, destCPF)
	w.drain(t)
	if again.ID != first.ID || again.State != pix.StateCompleted || w.ledger.debits != 1 || w.ledger.balance[w.source] != 70_000 {
		t.Fatalf("repetição: id igual=%v estado %v, débitos %d, saldo %d",
			again.ID == first.ID, again.State, w.ledger.debits, w.ledger.balance[w.source])
	}
}
