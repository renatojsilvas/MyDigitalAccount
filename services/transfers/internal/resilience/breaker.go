package resilience

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sony/gobreaker/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrNotSent indica que o circuito recusou a chamada antes de ela sair do processo: é a prova de
// "não enviado" que o transfers aceita na liquidação (ADR-0007, seção 5).
var ErrNotSent = errors.New("resilience: circuit open, request not sent")

// State é o estado do circuito; vira métrica (0 fechado, 1 meio-aberto, 2 aberto).
type State int

// Estados do circuito.
const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case HalfOpen:
		return "half_open"
	case Open:
		return "open"
	default:
		return "closed"
	}
}

// BreakerConfig são os limites do circuito (ADR-0007, seção 7).
type BreakerConfig struct {
	// Abre com ConsecutiveFailures falhas seguidas, ou com FailureRatio de falhas em pelo menos
	// MinRequests chamadas dentro de Window.
	ConsecutiveFailures uint32
	FailureRatio        float64
	MinRequests         uint32
	Window              time.Duration
	// OpenFor é quanto tempo fica aberto antes de deixar passar uma chamada de prova.
	OpenFor time.Duration
}

// DefaultBreaker são os valores do ADR-0007.
func DefaultBreaker() BreakerConfig {
	return BreakerConfig{ConsecutiveFailures: 5, FailureRatio: 0.5, MinRequests: 20, Window: 10 * time.Second, OpenFor: 5 * time.Second}
}

// Breaker é o circuit breaker de uma dependência, no modo de dois passos: primeiro a
// autorização, depois a chamada. Entre os dois, quem chama pode gravar algo (o marcador
// settlement_maybe_sent) sabendo que a chamada vai sair.
type Breaker struct {
	cb *gobreaker.TwoStepCircuitBreaker[struct{}]
}

// NewBreaker cria o circuito da dependência. onState é avisado a cada mudança (métrica).
func NewBreaker(dependency string, cfg BreakerConfig, onState func(dependency string, s State)) *Breaker {
	return &Breaker{cb: gobreaker.NewTwoStepCircuitBreaker[struct{}](gobreaker.Settings{
		Name:        dependency,
		MaxRequests: 1,
		Interval:    cfg.Window,
		Timeout:     cfg.OpenFor,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			if c.ConsecutiveFailures >= cfg.ConsecutiveFailures {
				return true
			}
			return c.Requests >= cfg.MinRequests && float64(c.TotalFailures)/float64(c.Requests) >= cfg.FailureRatio
		},
		IsSuccessful: func(err error) bool { return !InfraFailure(err) },
		// Cancelamento é decisão de quem chamou, não sinal sobre a dependência.
		IsExcluded: func(err error) bool { return errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled },
		OnStateChange: func(name string, _, to gobreaker.State) {
			onState(name, fromGobreaker(to))
		},
	})}
}

// Allow pede autorização. Recusada, devolve ErrNotSent: nada saiu. Autorizada, quem chama
// precisa chamar done com o resultado da chamada.
func (b *Breaker) Allow() (done func(error), err error) {
	done, err = b.cb.Allow()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotSent, b.cb.Name(), err)
	}
	return done, nil
}

// Do executa fn sob o circuito.
func (b *Breaker) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	done, err := b.Allow()
	if err != nil {
		return err
	}
	err = fn(ctx)
	done(err)
	return err
}

// State devolve o estado atual.
func (b *Breaker) State() State { return fromGobreaker(b.cb.State()) }

func fromGobreaker(s gobreaker.State) State {
	switch s {
	case gobreaker.StateHalfOpen:
		return HalfOpen
	case gobreaker.StateOpen:
		return Open
	default:
		return Closed
	}
}
