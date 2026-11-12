// Package resilience reúne as políticas de chamada às dependências do transfers (ADR-0007,
// seção 7): retry com backoff exponencial e full jitter, circuit breaker por dependência e a
// classificação dos erros gRPC (transitório, de negócio, cancelado).
package resilience

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Policy é a política de retry de uma chamada.
type Policy struct {
	// Attempts é o total de tentativas, contando a primeira.
	Attempts int
	// Base e Max limitam a espera: a n-ésima espera é sorteada entre 0 e min(Max, Base·2^n).
	Base, Max time.Duration
	// Timeout é o prazo de cada tentativa; zero usa só o prazo de quem chama.
	Timeout time.Duration
}

// Backoff devolve a espera antes da tentativa seguinte à de número attempt (0 = a primeira),
// com full jitter: sorteada entre zero e o teto exponencial. Sem o sorteio, todos os clientes
// que falharam juntos repetiriam juntos (thundering herd).
func (p Policy) Backoff(attempt int, roll func() float64) time.Duration {
	ceiling := p.Base << min(attempt, 30)
	if ceiling <= 0 || ceiling > p.Max {
		ceiling = p.Max
	}
	return time.Duration(roll() * float64(ceiling))
}

// Retry executa fn até dar certo, até o erro não ser repetível ou até acabarem as tentativas.
// onRetry é chamado antes de cada nova tentativa (métricas). Devolve o último erro.
func Retry(ctx context.Context, p Policy, retryable func(error) bool, onRetry func(err error), fn func(ctx context.Context) error) error {
	var err error
	for attempt := range max(p.Attempts, 1) {
		if attempt > 0 {
			onRetry(err)
			//nolint:gosec // jitter de espera, não segurança
			if werr := sleep(ctx, p.Backoff(attempt-1, rand.Float64)); werr != nil {
				return err
			}
		}
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if p.Timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, p.Timeout)
		}
		err = fn(callCtx)
		cancel()
		if err == nil || !retryable(err) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Transient diz se repetir pode dar certo: dependência fora do ar, prazo estourado, sobrecarga
// ou circuito aberto. Recusa de negócio nunca é transitória. Só é seguro repetir chamadas
// idempotentes; todas as do transfers são (chave por etapa, settlement_id, transfer_id).
func Transient(err error) bool {
	if errors.Is(err, ErrNotSent) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

// InfraFailure diz se o erro conta como falha para o circuit breaker: falhas da dependência, não
// recusas de negócio (NOT_FOUND, FAILED_PRECONDITION...), que provam que ela está respondendo.
func InfraFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Internal, codes.Unknown,
		codes.DataLoss, codes.Unimplemented:
		return true
	default:
		return false
	}
}
