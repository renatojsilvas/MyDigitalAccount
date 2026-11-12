package resilience_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"conta.local/transfers/internal/resilience"
)

func TestBackoffIsFullJitterUnderExponentialCeiling(t *testing.T) {
	p := resilience.Policy{Base: 100 * time.Millisecond, Max: time.Second}
	tests := []struct {
		attempt int
		roll    float64
		want    time.Duration
	}{
		{0, 1, 100 * time.Millisecond}, {1, 1, 200 * time.Millisecond}, {2, 1, 400 * time.Millisecond},
		{3, 1, 800 * time.Millisecond}, {4, 1, time.Second}, {40, 1, time.Second}, // teto
		{3, 0, 0}, {3, 0.5, 400 * time.Millisecond}, // sorteio entre zero e o teto
	}
	for _, tt := range tests {
		if got := p.Backoff(tt.attempt, func() float64 { return tt.roll }); got != tt.want {
			t.Errorf("Backoff(%d, %.1f) = %v, want %v", tt.attempt, tt.roll, got, tt.want)
		}
	}
}

func TestRetry(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "fora")
	business := status.Error(codes.FailedPrecondition, "saldo insuficiente")
	p := resilience.Policy{Attempts: 3, Base: time.Millisecond, Max: 2 * time.Millisecond}
	tests := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{"de primeira", []error{nil}, 1, nil},
		{"transitório e depois sucesso", []error{unavailable, nil}, 2, nil},
		{"transitório até esgotar", []error{unavailable, unavailable, unavailable}, 3, unavailable},
		{"recusa de negócio não repete", []error{business}, 1, business},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls, retries int
			err := resilience.Retry(t.Context(), p, resilience.Transient, func(error) { retries++ }, func(context.Context) error {
				calls++
				return tt.results[calls-1]
			})
			if calls != tt.wantCalls || !errors.Is(err, tt.wantErr) || retries != calls-1 {
				t.Fatalf("chamadas %d, retries %d, err %v; want %d, %d, %v", calls, retries, err, tt.wantCalls, tt.wantCalls-1, tt.wantErr)
			}
		})
	}
}

func TestRetryAppliesPerAttemptTimeout(t *testing.T) {
	p := resilience.Policy{Attempts: 2, Base: time.Millisecond, Max: time.Millisecond, Timeout: 20 * time.Millisecond}
	var calls int
	err := resilience.Retry(t.Context(), p, resilience.Transient, func(error) {}, func(ctx context.Context) error {
		calls++
		<-ctx.Done()
		return ctx.Err()
	})
	if calls != 2 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("chamadas %d, err %v; want 2 tentativas com prazo estourado", calls, err)
	}
}

func TestBreakerOpensOnInfraFailuresOnly(t *testing.T) {
	var states []resilience.State
	cfg := resilience.BreakerConfig{ConsecutiveFailures: 3, FailureRatio: 1, MinRequests: 100, Window: time.Minute, OpenFor: 50 * time.Millisecond}
	b := resilience.NewBreaker("spi", cfg, func(_ string, s resilience.State) { states = append(states, s) })
	fail := func(err error) error { return b.Do(t.Context(), func(context.Context) error { return err }) }

	// Recusas de negócio não abrem o circuito: provam que a dependência responde.
	for range 10 {
		_ = fail(status.Error(codes.NotFound, "não existe"))
	}
	if b.State() != resilience.Closed {
		t.Fatalf("estado %v depois de recusas de negócio", b.State())
	}
	for range 3 {
		_ = fail(status.Error(codes.Unavailable, "fora"))
	}
	if b.State() != resilience.Open {
		t.Fatalf("estado %v depois de 3 falhas seguidas", b.State())
	}
	// Aberto: a chamada nem sai, e o erro é a prova de "não enviado".
	called := false
	err := b.Do(t.Context(), func(context.Context) error { called = true; return nil })
	if called || !errors.Is(err, resilience.ErrNotSent) || !resilience.Transient(err) {
		t.Fatalf("circuito aberto: chamou=%v err=%v", called, err)
	}
	// Depois do tempo aberto, uma chamada de prova; com sucesso, fecha.
	time.Sleep(60 * time.Millisecond)
	if err := fail(nil); err != nil {
		t.Fatalf("chamada de prova: %v", err)
	}
	if b.State() != resilience.Closed {
		t.Fatalf("estado %v depois da prova com sucesso", b.State())
	}
	want := []resilience.State{resilience.Open, resilience.HalfOpen, resilience.Closed}
	if len(states) != len(want) || states[0] != want[0] || states[1] != want[1] || states[2] != want[2] {
		t.Fatalf("mudanças de estado = %v, want %v", states, want)
	}
}

func TestClassification(t *testing.T) {
	tests := []struct {
		err              error
		transient, infra bool
	}{
		{status.Error(codes.Unavailable, ""), true, true},
		{status.Error(codes.DeadlineExceeded, ""), true, true},
		{context.DeadlineExceeded, true, true},
		{status.Error(codes.Internal, ""), false, true},
		{status.Error(codes.FailedPrecondition, ""), false, false},
		{status.Error(codes.NotFound, ""), false, false},
		{status.Error(codes.AlreadyExists, ""), false, false},
	}
	for _, tt := range tests {
		if resilience.Transient(tt.err) != tt.transient || resilience.InfraFailure(tt.err) != tt.infra {
			t.Errorf("%v: transitório=%v infra=%v; want %v %v", tt.err, resilience.Transient(tt.err), resilience.InfraFailure(tt.err), tt.transient, tt.infra)
		}
	}
}
