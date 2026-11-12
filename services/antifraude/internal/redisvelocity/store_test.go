package redisvelocity_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"conta.local/antifraude/internal/redisvelocity"
	"conta.local/antifraude/internal/risk"
)

// client conecta ao Redis real de teste (ADR-0003). Cada teste usa contas e Pix novos (UUID),
// então as chaves não colidem entre testes em paralelo.
func client(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("Redis de teste não configurado (TEST_REDIS_ADDR): rode pelo contêiner tools")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func request(t *testing.T, account uuid.UUID, cents int64) risk.Request {
	t.Helper()
	r, err := risk.NewRequest(uuid.NewString(), account.String(), cents)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecordCountsAndSums(t *testing.T) {
	rdb := client(t)
	s := redisvelocity.New(rdb)
	now := time.Date(2026, 11, 12, 14, 10, 0, 0, time.UTC)
	account := uuid.New()

	var last risk.Counters
	for _, cents := range []int64{1_000, 2_500, 500} {
		var err error
		if last, err = s.Record(t.Context(), request(t, account, cents), now); err != nil {
			t.Fatal(err)
		}
	}
	if last != (risk.Counters{CurrentCount: 3, CurrentSum: 4_000}) {
		t.Fatalf("contadores = %+v, want 3 Pix e 4000", last)
	}

	// Outra conta não enxerga os contadores desta.
	other, err := s.Record(t.Context(), request(t, uuid.New(), 100), now)
	if err != nil || other != (risk.Counters{CurrentCount: 1, CurrentSum: 100}) {
		t.Fatalf("outra conta = %+v, %v", other, err)
	}
}

// O retry do transfers repete o Evaluate com o mesmo Pix: não pode contar de novo.
func TestRecordIsIdempotentByTransfer(t *testing.T) {
	s := redisvelocity.New(client(t))
	now := time.Date(2026, 11, 12, 14, 10, 0, 0, time.UTC)
	req := request(t, uuid.New(), 1_000)

	first, err := s.Record(t.Context(), req, now)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		again, err := s.Record(t.Context(), req, now)
		if err != nil || again != first {
			t.Fatalf("repetição = %+v, %v; want %+v", again, err, first)
		}
	}
}

func TestRecordSeesPreviousWindow(t *testing.T) {
	s := redisvelocity.New(client(t))
	account := uuid.New()
	before := time.Date(2026, 11, 12, 13, 50, 0, 0, time.UTC)
	after := before.Add(20 * time.Minute) // 14:10, a janela seguinte

	if _, err := s.Record(t.Context(), request(t, account, 7_000), before); err != nil {
		t.Fatal(err)
	}
	c, err := s.Record(t.Context(), request(t, account, 1_000), after)
	if err != nil {
		t.Fatal(err)
	}
	if c != (risk.Counters{CurrentCount: 1, CurrentSum: 1_000, PreviousCount: 1, PreviousSum: 7_000}) {
		t.Fatalf("contadores = %+v", c)
	}
}

func TestKeysExpire(t *testing.T) {
	rdb := client(t)
	s := redisvelocity.New(rdb)
	now := time.Now()
	req := request(t, uuid.New(), 1_000)
	if _, err := s.Record(t.Context(), req, now); err != nil {
		t.Fatal(err)
	}
	// As três chaves escritas (marca, quantidade e soma da janela atual) têm expiração.
	for _, key := range redisvelocity.Keys(req, now)[:3] {
		ttl, err := rdb.TTL(t.Context(), key).Result()
		if err != nil || ttl <= time.Hour || ttl > 2*time.Hour {
			t.Fatalf("TTL de %s = %v, %v; want entre 1 h e 2 h", key, ttl, err)
		}
	}
}

// Atomicidade: Pix concorrentes da mesma conta, cada um contado exatamente uma vez.
func TestRecordIsAtomicUnderConcurrency(t *testing.T) {
	s := redisvelocity.New(client(t))
	now := time.Date(2026, 11, 12, 14, 10, 0, 0, time.UTC)
	account := uuid.New()

	const n = 50
	reqs := make([]risk.Request, n)
	for i := range reqs {
		reqs[i] = request(t, account, 100)
	}
	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := range 2 * n { // cada Pix enviado duas vezes, como num retry
		wg.Go(func() {
			_, errs[i] = s.Record(context.Background(), reqs[i%n], now)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	c, err := s.Record(t.Context(), reqs[0], now)
	if err != nil || c.CurrentCount != n || c.CurrentSum != n*100 {
		t.Fatalf("contadores = %+v, %v; want %d Pix e %d", c, err, n, n*100)
	}
}
