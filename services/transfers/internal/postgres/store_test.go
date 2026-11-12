package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/postgres"
	"conta.local/transfers/internal/postgres/pgtest"
)

const dailyLimit = 200_000

type fixture struct {
	pool  *pgxpool.Pool
	store *postgres.Store
	svc   *pix.Service
	now   time.Time
}

func newFixture(t *testing.T, now time.Time) *fixture {
	t.Helper()
	_, pool := pgtest.Migrated(t)
	cal, err := pix.NewCalendar()
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, store: postgres.NewStore(pool), now: now}
	f.svc = pix.NewService(f.store, cal, []byte("segredo-de-teste-com-32-bytes-ok"), dailyLimit,
		pix.WithClock(func() time.Time { return f.now }))
	return f
}

func (f *fixture) start(t *testing.T, account uuid.UUID, key string, cents int64) pix.Transfer {
	t.Helper()
	cmd, err := pix.NewStartCommand(key, account.String(), pix.KeyCPF, "12345678909", cents, "aluguel")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Trace = map[string]string{"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}
	tr, err := f.svc.Start(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return tr
}

func (f *fixture) lease(t *testing.T, id uuid.UUID) pix.Lease {
	t.Helper()
	l, err := f.store.Acquire(t.Context(), id, "teste", time.Minute)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return l
}

func (f *fixture) used(t *testing.T, account uuid.UUID, day pix.Day) int64 {
	t.Helper()
	u, err := f.store.UsedOn(t.Context(), account, day)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAcceptAndGet(t *testing.T) {
	f := newFixture(t, time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	account := uuid.New()
	tr := f.start(t, account, "k1", 5_000)

	got, history, err := f.store.Get(t.Context(), account, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != tr.ID || got.State != pix.StateAccepted || got.SettlementID != tr.SettlementID ||
		got.DestinationKey.Value() != "12345678909" || got.AmountCents != 5_000 || !got.CreatedAt.Equal(tr.CreatedAt) ||
		got.Trace["traceparent"] == "" || got.LimitDay != tr.LimitDay {
		t.Fatalf("lido = %+v\naceito = %+v", got, tr)
	}
	if len(history) != 1 || history[0].From != 0 || history[0].To != pix.StateAccepted {
		t.Fatalf("histórico = %+v", history)
	}
	// De outra conta: NOT_FOUND, sem revelar que existe.
	if _, _, err := f.store.Get(t.Context(), uuid.New(), tr.ID); !errors.Is(err, pix.ErrNotFound) {
		t.Fatalf("outra conta: %v, want ErrNotFound", err)
	}
}

// A mesma chave enviada várias vezes ao mesmo tempo (retries do BFF): um único Pix.
func TestConcurrentAcceptSameKey(t *testing.T) {
	f := newFixture(t, time.Now())
	account := uuid.New()
	const n = 10
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			cmd, _ := pix.NewStartCommand("mesma", account.String(), pix.KeyCPF, "12345678909", 100, "")
			tr, err := f.svc.Start(context.Background(), cmd)
			ids[i], errs[i] = tr.ID, err
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || ids[i] != ids[0] {
			t.Fatalf("chamada %d: %v, id %s (primeiro %s)", i, errs[i], ids[i], ids[0])
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), "SELECT count(*) FROM transfers WHERE source_account_id = $1", account).Scan(&count); err != nil || count != 1 {
		t.Fatalf("%d transferências, %v; want 1", count, err)
	}
	// Outra conta pode usar a mesma chave: o escopo é a conta.
	other := f.start(t, uuid.New(), "mesma", 100)
	if other.ID == ids[0] {
		t.Fatal("a chave de outra conta devolveu o Pix desta")
	}
}

func TestKeyReusedWithDifferentRequest(t *testing.T) {
	f := newFixture(t, time.Now())
	account := uuid.New()
	f.start(t, account, "k", 100)
	cmd, _ := pix.NewStartCommand("k", account.String(), pix.KeyCPF, "12345678909", 200, "aluguel")
	if _, err := f.svc.Start(t.Context(), cmd); !errors.Is(err, pix.ErrIdempotencyKeyReused) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
	}
}

// A tabela de idempotência não guarda dado pessoal: nem o CPF de destino nem nada dele.
func TestIdempotencyTableHasNoPersonalData(t *testing.T) {
	f := newFixture(t, time.Now())
	f.start(t, uuid.New(), "k", 100)
	var row string
	if err := f.pool.QueryRow(t.Context(),
		"SELECT row_to_json(k)::text || encode(request_hmac, 'hex') FROM transfer_idempotency_keys k").Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"12345678909", "456789", "aluguel"} {
		if strings.Contains(row, leak) {
			t.Fatalf("a linha de idempotência contém %q: %s", leak, row)
		}
	}
}

// Pix simultâneos da mesma conta: a reserva é atômica, e nunca passam juntos do limite.
func TestConcurrentReservationsNeverExceedLimit(t *testing.T) {
	f := newFixture(t, time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	account := uuid.New()
	const n = 10 // 10 x R$ 300 contra R$ 2.000: passam exatamente 6
	leases := make([]pix.Lease, n)
	for i := range n {
		leases[i] = f.lease(t, f.start(t, account, uuid.NewString(), 30_000).ID)
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = f.store.ReserveLimit(context.Background(), leases[i], dailyLimit, f.now) })
	}
	wg.Wait()
	var ok, exceeded int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, pix.ErrLimitExceeded):
			exceeded++
		default:
			t.Fatal(err)
		}
	}
	day := pix.Day{Year: 2026, Month: 11, D: 12}
	if ok != 6 || exceeded != 4 || f.used(t, account, day) != 180_000 {
		t.Fatalf("reservas %d, recusas %d, usado %d; want 6, 4, 180000", ok, exceeded, f.used(t, account, day))
	}
	// Reservar de novo a mesma transferência não reserva duas vezes.
	if err := f.store.ReserveLimit(t.Context(), leases[0], dailyLimit, f.now); err != nil && !errors.Is(err, pix.ErrLimitExceeded) {
		t.Fatal(err)
	}
	if f.used(t, account, day) != 180_000 {
		t.Fatalf("reserva repetida mudou o uso: %d", f.used(t, account, day))
	}
}

// Ajuste do autor ao ADR-0007: o Pix das 23h59 estornado às 00h05 devolve o limite ao dia da
// reserva, não ao dia corrente.
func TestReleaseGoesToReservationDay(t *testing.T) {
	at2359 := time.Date(2026, 11, 13, 2, 59, 0, 0, time.UTC) // 23:59 de 12/11 em São Paulo
	f := newFixture(t, at2359)
	account := uuid.New()
	tr := f.start(t, account, "k", 50_000)
	l := f.lease(t, tr.ID)
	if err := f.store.ReserveLimit(t.Context(), l, dailyLimit, f.now); err != nil {
		t.Fatal(err)
	}

	// Um Pix do dia seguinte já reservou limite no dia 13.
	f.now = at2359.Add(6 * time.Minute) // 00:05 de 13/11
	next := f.start(t, account, "k2", 10_000)
	if err := f.store.ReserveLimit(t.Context(), f.lease(t, next.ID), dailyLimit, f.now); err != nil {
		t.Fatal(err)
	}

	// A saga do primeiro anda até o estorno, às 00:05.
	steps := []pix.Transition{
		{From: pix.StateAccepted, To: pix.StateValidated, DestinationAccountID: uuid.New()},
		{From: pix.StateValidated, To: pix.StateDebited, DebitEntryID: uuid.New()},
		{From: pix.StateDebited, To: pix.StateSettling},
		{From: pix.StateSettling, To: pix.StateReversing, Reason: pix.ReasonSettlementRejected},
		{From: pix.StateReversing, To: pix.StateReversed, Reason: pix.ReasonSettlementRejected},
	}
	for _, s := range steps {
		s.At, s.NextRunAt = f.now, f.now
		if err := f.store.Transition(t.Context(), l, s); err != nil {
			t.Fatalf("%s -> %s: %v", s.From, s.To, err)
		}
	}

	day12, day13 := pix.Day{Year: 2026, Month: 11, D: 12}, pix.Day{Year: 2026, Month: 11, D: 13}
	if u := f.used(t, account, day12); u != 0 {
		t.Fatalf("uso do dia 12 = %d, want 0 (devolvido ao dia da reserva)", u)
	}
	if u := f.used(t, account, day13); u != 10_000 {
		t.Fatalf("uso do dia 13 = %d, want 10000 (intacto)", u)
	}
	got, history, err := f.store.Get(t.Context(), account, tr.ID)
	if err != nil || got.State != pix.StateReversed || got.Reason != pix.ReasonSettlementRejected || got.LimitReserved || len(history) != 6 {
		t.Fatalf("estado final = %+v (%d mudanças), %v", got, len(history), err)
	}
}

// A recusa antes do débito também devolve a reserva, uma vez só.
func TestRejectionReleasesOnce(t *testing.T) {
	f := newFixture(t, time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC))
	account := uuid.New()
	tr := f.start(t, account, "k", 30_000)
	l := f.lease(t, tr.ID)
	if err := f.store.ReserveLimit(t.Context(), l, dailyLimit, f.now); err != nil {
		t.Fatal(err)
	}
	reject := pix.Transition{From: pix.StateAccepted, To: pix.StateRejected, Reason: pix.ReasonAntifraudDenied, At: f.now, NextRunAt: f.now}
	if err := f.store.Transition(t.Context(), l, reject); err != nil {
		t.Fatal(err)
	}
	if u := f.used(t, account, tr.LimitDay); u != 0 {
		t.Fatalf("uso = %d, want 0", u)
	}
	// Repetir a transição falha (o estado já é outro) e não devolve de novo.
	if err := f.store.Transition(t.Context(), l, reject); err == nil {
		t.Fatal("transição repetida aceita")
	}
	if u := f.used(t, account, tr.LimitDay); u != 0 {
		t.Fatalf("uso = %d depois da repetição", u)
	}
}

// Fencing token: um runner cujo lease foi tomado por outro não grava nada (ADR-0007).
func TestStaleLeaseCannotWrite(t *testing.T) {
	f := newFixture(t, time.Now())
	tr := f.start(t, uuid.New(), "k", 100)
	old, err := f.store.Acquire(t.Context(), tr.ID, "runner-a", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// O lease de A expira; B toma.
	var fresh pix.Lease
	deadline := time.Now().Add(2 * time.Second)
	for {
		if fresh, err = f.store.Acquire(t.Context(), tr.ID, "runner-b", time.Minute); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B não tomou o lease expirado: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fresh.Token <= old.Token {
		t.Fatalf("token novo %d não é maior que o antigo %d", fresh.Token, old.Token)
	}
	// Com o lease de B ativo, ninguém mais o toma.
	if _, err := f.store.Acquire(t.Context(), tr.ID, "runner-c", time.Minute); !errors.Is(err, pix.ErrLeaseLost) {
		t.Fatalf("C tomou um lease ativo: %v", err)
	}

	tr1 := pix.Transition{From: pix.StateAccepted, To: pix.StateValidated, DestinationAccountID: uuid.New(), At: f.now, NextRunAt: f.now}
	if err := f.store.Transition(t.Context(), old, tr1); !errors.Is(err, pix.ErrLeaseLost) {
		t.Fatalf("A (lease antigo) transitou: %v, want ErrLeaseLost", err)
	}
	if err := f.store.ReserveLimit(t.Context(), old, dailyLimit, f.now); !errors.Is(err, pix.ErrLeaseLost) {
		t.Fatalf("A (lease antigo) reservou: %v, want ErrLeaseLost", err)
	}
	if err := f.store.Transition(t.Context(), fresh, tr1); err != nil {
		t.Fatalf("B não transitou: %v", err)
	}
}

func TestInvalidTransitionNeverReachesTheDatabase(t *testing.T) {
	f := newFixture(t, time.Now())
	tr := f.start(t, uuid.New(), "k", 100)
	l := f.lease(t, tr.ID)
	bad := pix.Transition{From: pix.StateAccepted, To: pix.StateCompleted, At: f.now, NextRunAt: f.now}
	if err := f.store.Transition(t.Context(), l, bad); !errors.Is(err, pix.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code + " " + pgErr.ConstraintName
	}
	return ""
}

// settlement_maybe_sent é irreversível também no banco (ADR-0007, seção 5).
func TestMaybeSentIsSticky(t *testing.T) {
	f := newFixture(t, time.Now())
	tr := f.start(t, uuid.New(), "k", 100)
	if _, err := f.pool.Exec(t.Context(), "UPDATE transfers SET settlement_maybe_sent = true WHERE id = $1", tr.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.pool.Exec(t.Context(), "UPDATE transfers SET settlement_maybe_sent = false WHERE id = $1", tr.ID)
	if got := pgCode(err); got != "23514 transfers_maybe_sent_sticky" {
		t.Fatalf("err = %v (%s), want check_violation transfers_maybe_sent_sticky", err, got)
	}
}

// O papel do serviço não altera o que é imutável (valor, origem, settlement_id) nem apaga.
func TestServiceRolePrivileges(t *testing.T) {
	f := newFixture(t, time.Now())
	f.start(t, uuid.New(), "k", 100)
	for _, sql := range []string{
		"UPDATE transfers SET amount_cents = 1",
		"UPDATE transfers SET settlement_id = gen_random_uuid()",
		"UPDATE transfers SET limit_day = limit_day + 1",
		"DELETE FROM transfers",
		"DELETE FROM transfer_state_history",
		"UPDATE transfer_idempotency_keys SET request_hmac = request_hmac",
	} {
		if _, err := f.pool.Exec(t.Context(), sql); !strings.HasPrefix(pgCode(err), "42501") {
			t.Errorf("%s: err = %v, want 42501 (sem privilégio)", sql, err)
		}
	}
}
