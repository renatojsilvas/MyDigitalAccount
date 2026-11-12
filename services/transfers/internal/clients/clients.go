// Package clients são os adaptadores gRPC do transfers para antifraude, ledger e SPI: traduzem
// o contrato (status gRPC e google.rpc.ErrorInfo) para os erros das portas da saga, e aplicam
// retry com backoff e jitter e um circuit breaker por dependência (ADR-0007, seção 7).
package clients

import (
	"context"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"

	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

// Observer recebe retries e mudanças de estado do circuito, por dependência (métricas).
type Observer interface {
	Retried(dependency string)
	BreakerState(dependency string, s resilience.State)
}

// Dependency agrupa o circuito e a política de retry de uma dependência.
type Dependency struct {
	Name    string
	Breaker *resilience.Breaker
	Policy  resilience.Policy
	obs     Observer
}

// NewDependency cria o circuito da dependência, ligado ao Observer.
func NewDependency(name string, cfg resilience.BreakerConfig, policy resilience.Policy, obs Observer) *Dependency {
	return &Dependency{
		Name: name, Policy: policy, obs: obs,
		Breaker: resilience.NewBreaker(name, cfg, func(dep string, s resilience.State) { obs.BreakerState(dep, s) }),
	}
}

// call executa fn com retry (só erros transitórios) e circuito em cada tentativa.
func (d *Dependency) call(ctx context.Context, fn func(ctx context.Context) error) error {
	return resilience.Retry(ctx, d.Policy, resilience.Transient, func(error) { d.obs.Retried(d.Name) },
		func(ctx context.Context) error { return d.Breaker.Do(ctx, fn) })
}

// unavailable embrulha uma falha da dependência no erro da porta, mantendo a causa.
func (d *Dependency) unavailable(err error) error {
	return fmt.Errorf("%w: %s: %w", saga.ErrUnavailable, d.Name, err)
}

// reasonOf lê o motivo de domínio do erro (google.rpc.ErrorInfo.reason), ou "".
func reasonOf(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason()
		}
	}
	return ""
}
