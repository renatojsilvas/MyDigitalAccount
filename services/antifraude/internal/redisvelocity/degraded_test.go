package redisvelocity_test

import (
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"conta.local/antifraude/internal/redisvelocity"
	"conta.local/antifraude/internal/risk"
	"conta.local/testkit/toxi"
)

// Redis fora do ar e Redis lento (ADR-0008, seção 3), com o Redis real atrás do Toxiproxy:
// o antifraude responde degradado, rápido, e volta ao normal quando o Redis volta.
func TestEvaluateWithRedisFailures(t *testing.T) {
	upstream := os.Getenv("TEST_REDIS_ADDR")
	if upstream == "" {
		t.Skip("Redis de teste não configurado (TEST_REDIS_ADDR)")
	}
	const timeout = 50 * time.Millisecond
	proxy := toxi.New(t, upstream)
	rdb := redisvelocity.NewClient(proxy.Addr, timeout)
	t.Cleanup(func() { _ = rdb.Close() })
	svc := risk.NewService(redisvelocity.New(rdb), risk.DefaultRules(), timeout)
	account := uuid.New()

	evaluate := func(t *testing.T, cents int64) (risk.Assessment, time.Duration) {
		t.Helper()
		req, err := risk.NewRequest(uuid.NewString(), account.String(), cents)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		a, err := svc.Evaluate(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		return a, time.Since(start)
	}

	// Espera o Redis responder sem degradar. A primeira chamada abre a conexão e carrega o
	// script (EVALSHA cai para EVAL) dentro dos 50 ms; com a CPU disputada (-race, pacotes em
	// paralelo) isso pode não caber, e não é o que este teste prova.
	waitHealthy := func(t *testing.T) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			a, _ := evaluate(t, 1_000)
			if !a.Degraded {
				if a.Decision != risk.Approve {
					t.Fatalf("Redis no ar: %+v, want aprovado", a)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("continua degradado com o Redis no ar")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitHealthy(t)

	// O que se prova: a decisão degradada não espera o Redis. O limite tem folga de 10x sobre o
	// prazo de 50 ms, e a latência injetada (1 s) é o dobro dele: se o antifraude esperasse o
	// Redis lento, estouraria.
	const maxDegraded = 500 * time.Millisecond

	failures := []struct {
		name   string
		inject func(t testing.TB)
		heal   func(t testing.TB)
	}{
		{"fora do ar", proxy.Disable, proxy.Enable},
		{"lento (1 s por resposta)", func(t testing.TB) { proxy.Latency(t, time.Second) }, proxy.RemoveToxics},
	}
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			f.inject(t)
			low, took := evaluate(t, 50_000)
			if !low.Degraded || low.Decision != risk.Approve {
				t.Fatalf("até o teto: %+v, want aprovado degradado", low)
			}
			if took > maxDegraded {
				t.Fatalf("decisão degradada levou %v; want menos de %v", took, maxDegraded)
			}
			if high, _ := evaluate(t, 50_001); !high.Degraded || high.Decision != risk.Deny {
				t.Fatalf("acima do teto: %+v, want recusado degradado", high)
			}

			f.heal(t)
			// O pool pode ter conexões quebradas da falha: a primeira chamada pode ainda falhar.
			waitHealthy(t)
		})
	}
}
