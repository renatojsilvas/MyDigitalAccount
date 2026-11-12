package risk_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"conta.local/antifraude/internal/risk"
)

func TestScore(t *testing.T) {
	r := risk.DefaultRules()
	tests := []struct {
		name         string
		amount       int64
		velocity     risk.Velocity
		wantScore    int
		wantDecision risk.Decision
		wantSignals  []risk.Signal
	}{
		{"primeiro Pix, valor baixo", 10_000, risk.Velocity{Count: 1, SumCents: 10_000}, 0, risk.Approve, nil},
		{"valor alto sozinho aprova", 150_000, risk.Velocity{Count: 1, SumCents: 150_000}, 20, risk.Approve,
			[]risk.Signal{risk.SignalHighAmount}},
		{"no limite do valor alto não pontua", 100_000, risk.Velocity{Count: 1, SumCents: 100_000}, 0, risk.Approve, nil},
		{"5 Pix na hora", 1_000, risk.Velocity{Count: 5, SumCents: 5_000}, 20, risk.Approve,
			[]risk.Signal{risk.SignalHighVelocityCount}},
		{"10 Pix na hora", 1_000, risk.Velocity{Count: 10, SumCents: 10_000}, 40, risk.Approve,
			[]risk.Signal{risk.SignalHighVelocityCount}},
		{"soma acima de R$ 2.000", 50_000, risk.Velocity{Count: 2, SumCents: 200_001}, 20, risk.Approve,
			[]risk.Signal{risk.SignalHighVelocityAmount}},
		{"valor alto e soma acima de R$ 5.000: recusa", 150_000, risk.Velocity{Count: 4, SumCents: 600_000}, 60, risk.Deny,
			[]risk.Signal{risk.SignalHighAmount, risk.SignalHighVelocityAmount}},
		{"tudo alto: teto de 100", 150_000, risk.Velocity{Count: 12, SumCents: 900_000}, 100, risk.Deny,
			[]risk.Signal{risk.SignalHighAmount, risk.SignalHighVelocityCount, risk.SignalHighVelocityAmount}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := r.Score(tt.amount, tt.velocity)
			if a.Score != tt.wantScore || a.Decision != tt.wantDecision || !slices.Equal(a.Signals, tt.wantSignals) || a.Degraded {
				t.Fatalf("= %+v, want score %d, %v, sinais %v", a, tt.wantScore, tt.wantDecision, tt.wantSignals)
			}
		})
	}
}

func TestDegraded(t *testing.T) {
	r := risk.DefaultRules()
	tests := []struct {
		name         string
		amount       int64
		wantDecision risk.Decision
		wantSignals  []risk.Signal
	}{
		{"até R$ 500 aprova", 50_000, risk.Approve, nil},
		{"acima de R$ 500 recusa", 50_001, risk.Deny, []risk.Signal{risk.SignalVelocityUnavailable}},
		{"valor alto recusa com os dois sinais", 150_000, risk.Deny,
			[]risk.Signal{risk.SignalHighAmount, risk.SignalVelocityUnavailable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := r.Degraded(tt.amount)
			if !a.Degraded || a.Decision != tt.wantDecision || !slices.Equal(a.Signals, tt.wantSignals) {
				t.Fatalf("= %+v, want %v com %v", a, tt.wantDecision, tt.wantSignals)
			}
		})
	}
}

func TestEstimate(t *testing.T) {
	c := risk.Counters{CurrentCount: 2, CurrentSum: 2_000, PreviousCount: 10, PreviousSum: 100_000}
	base := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		now       time.Time
		wantCount int64
		wantSum   int64
	}{
		{"início da janela: a anterior conta inteira", base, 12, 102_000},
		{"meio da janela: metade da anterior", base.Add(30 * time.Minute), 7, 52_000},
		{"um segundo antes do fim: quase nada da anterior", base.Add(time.Hour - time.Second), 2, 2_027},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := c.Estimate(tt.now)
			if v.Count != tt.wantCount || v.SumCents != tt.wantSum {
				t.Fatalf("= %+v, want %d e %d", v, tt.wantCount, tt.wantSum)
			}
		})
	}
}

func TestNewRequest(t *testing.T) {
	id := uuid.NewString()
	tests := []struct {
		name, transfer, account string
		amount                  int64
		wantField               string
	}{
		{"válido", id, id, 1, ""},
		{"transfer_id inválido", "x", id, 1, "transfer_id"},
		{"account_id inválido", id, "", 1, "account_id"},
		{"valor zero", id, id, 0, "amount_cents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := risk.NewRequest(tt.transfer, tt.account, tt.amount)
			var verr *risk.ValidationError
			if tt.wantField == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.As(err, &verr) || verr.Field != tt.wantField {
				t.Fatalf("err = %v, want campo %s", err, tt.wantField)
			}
		})
	}
}

func TestRulesValidate(t *testing.T) {
	if err := risk.DefaultRules().Validate(); err != nil {
		t.Fatalf("padrão inválido: %v", err)
	}
	bad := risk.DefaultRules()
	bad.VelocityCountHigh = 1
	bad.DenyScore = 0
	if err := bad.Validate(); err == nil {
		t.Fatal("limites incoerentes aceitos")
	}
}

// fakeStore devolve contadores fixos ou um erro, e pode demorar.
type fakeStore struct {
	counters risk.Counters
	err      error
	delay    time.Duration
}

func (f fakeStore) Record(ctx context.Context, _ risk.Request, _ time.Time) (risk.Counters, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return risk.Counters{}, ctx.Err()
		}
	}
	return f.counters, f.err
}

type recorder struct{ reasons []risk.DegradedReason }

func (r *recorder) Evaluated(_ context.Context, _ risk.Request, _ risk.Assessment, reason risk.DegradedReason) {
	r.reasons = append(r.reasons, reason)
}

func TestServiceEvaluate(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	req, err := risk.NewRequest(uuid.NewString(), uuid.NewString(), 60_000)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		store        fakeStore
		wantDegraded bool
		wantReason   risk.DegradedReason
		wantDecision risk.Decision
	}{
		{"com contadores", fakeStore{counters: risk.Counters{CurrentCount: 1, CurrentSum: 60_000}}, false, "", risk.Approve},
		{"Redis com erro: degrada e recusa acima do teto", fakeStore{err: errors.New("conexão recusada")}, true, risk.DegradedError, risk.Deny},
		{"Redis lento: degrada por timeout", fakeStore{delay: time.Second}, true, risk.DegradedTimeout, risk.Deny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			svc := risk.NewService(tt.store, risk.DefaultRules(), 20*time.Millisecond,
				risk.WithClock(func() time.Time { return now }), risk.WithObserver(rec))
			start := time.Now()
			a, err := svc.Evaluate(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if a.Degraded != tt.wantDegraded || a.Decision != tt.wantDecision {
				t.Fatalf("= %+v, want degradado=%v %v", a, tt.wantDegraded, tt.wantDecision)
			}
			if len(rec.reasons) != 1 || rec.reasons[0] != tt.wantReason {
				t.Fatalf("observador = %v, want [%q]", rec.reasons, tt.wantReason)
			}
			if time.Since(start) > 500*time.Millisecond {
				t.Fatalf("demorou %v: o Pix não pode esperar o Redis", time.Since(start))
			}
		})
	}
}

func TestServiceEvaluateCallerGaveUp(t *testing.T) {
	req, _ := risk.NewRequest(uuid.NewString(), uuid.NewString(), 1)
	svc := risk.NewService(fakeStore{delay: time.Second}, risk.DefaultRules(), time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, err := svc.Evaluate(ctx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded (o prazo de quem chamou acabou)", err)
	}
}
