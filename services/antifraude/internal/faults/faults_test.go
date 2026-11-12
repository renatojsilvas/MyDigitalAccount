package faults_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"conta.local/antifraude/internal/faults"
)

func TestApply(t *testing.T) {
	tests := []struct {
		name    string
		cfg     faults.Config
		roll    float64
		wantErr error
		minWait time.Duration
	}{
		{"sem falhas", faults.Config{}, 0, nil, 0},
		{"taxa 1 sempre falha", faults.Config{ErrorRate: 1}, 0.999, faults.ErrInjected, 0},
		{"sorteio acima da taxa passa", faults.Config{ErrorRate: 0.3}, 0.5, nil, 0},
		{"sorteio abaixo da taxa falha", faults.Config{ErrorRate: 0.3}, 0.1, faults.ErrInjected, 0},
		{"latência", faults.Config{Latency: 30 * time.Millisecond}, 0, nil, 30 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inj := faults.New(tt.cfg).WithRoll(func() float64 { return tt.roll })
			start := time.Now()
			if err := inj.Apply(t.Context()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if time.Since(start) < tt.minWait {
				t.Fatalf("esperou %v, want pelo menos %v", time.Since(start), tt.minWait)
			}
		})
	}
}

func TestApplyRespectsCallerDeadline(t *testing.T) {
	inj := faults.New(faults.Config{Latency: 10 * time.Second})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := inj.Apply(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestSetValidates(t *testing.T) {
	inj := faults.New(faults.Config{})
	for _, bad := range []faults.Config{
		{ErrorRate: -0.1}, {ErrorRate: 1.1}, {ErrorRate: math.NaN()},
		{Latency: -time.Second}, {Latency: faults.MaxLatency + time.Second},
	} {
		if err := inj.Set(bad); err == nil {
			t.Errorf("aceitou %+v", bad)
		}
	}
	if inj.Current() != (faults.Config{}) {
		t.Fatalf("configuração inválida foi aplicada: %+v", inj.Current())
	}
	good := faults.Config{Latency: time.Second, ErrorRate: 0.5}
	if err := inj.Set(good); err != nil || inj.Current() != good {
		t.Fatalf("Set(%+v) = %v, atual %+v", good, err, inj.Current())
	}
}
