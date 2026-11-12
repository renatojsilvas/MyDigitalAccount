package risk

import (
	"context"
	"errors"
	"time"
)

// VelocityStore conta o Pix nos contadores da conta e devolve os contadores das janelas atual e
// anterior. Idempotente por TransferID: repetir não conta de novo. Implementado sobre o Redis
// (internal/redisvelocity).
type VelocityStore interface {
	Record(ctx context.Context, req Request, now time.Time) (Counters, error)
}

// DegradedReason diz por que a avaliação foi degradada; vira rótulo de métrica (conjunto fixo).
type DegradedReason string

// Motivos de degradação.
const (
	DegradedTimeout DegradedReason = "timeout"
	DegradedError   DegradedReason = "error"
)

// Observer recebe cada avaliação, para métricas e atributos de trace. O adaptador de
// telemetria o implementa; o domínio não conhece OpenTelemetry.
type Observer interface {
	Evaluated(ctx context.Context, req Request, a Assessment, reason DegradedReason)
}

type noopObserver struct{}

func (noopObserver) Evaluated(context.Context, Request, Assessment, DegradedReason) {}

// Service é o caso de uso Evaluate.
type Service struct {
	store        VelocityStore
	rules        Rules
	storeTimeout time.Duration
	now          func() time.Time
	observer     Observer
}

// Option configura o Service (functional options).
type Option func(*Service)

// WithClock troca o relógio, para testes determinísticos.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithObserver liga o Service a um Observer.
func WithObserver(o Observer) Option { return func(s *Service) { s.observer = o } }

// NewService monta o caso de uso. storeTimeout limita a espera pelos contadores: estourado, a
// avaliação segue degradada (o Pix não espera o Redis).
func NewService(store VelocityStore, rules Rules, storeTimeout time.Duration, opts ...Option) *Service {
	s := &Service{store: store, rules: rules, storeTimeout: storeTimeout, now: time.Now, observer: noopObserver{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Evaluate avalia o Pix. Nunca falha por causa dos contadores: sem eles, decide degradado
// (ADR-0008, seção 3). Só devolve erro se o contexto de quem chamou acabou.
func (s *Service) Evaluate(ctx context.Context, req Request) (Assessment, error) {
	now := s.now()
	storeCtx, cancel := context.WithTimeout(ctx, s.storeTimeout)
	counters, err := s.store.Record(storeCtx, req, now)
	cancel()

	if err != nil {
		// O prazo de quem chamou acabou: não há a quem responder.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Assessment{}, ctxErr
		}
		reason := DegradedError
		if errors.Is(err, context.DeadlineExceeded) {
			reason = DegradedTimeout
		}
		a := s.rules.Degraded(req.AmountCents)
		s.observer.Evaluated(ctx, req, a, reason)
		return a, nil
	}
	a := s.rules.Score(req.AmountCents, counters.Estimate(now))
	s.observer.Evaluated(ctx, req, a, "")
	return a, nil
}
