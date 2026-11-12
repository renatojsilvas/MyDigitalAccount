// Package risk é o domínio do antifraude (ADR-0008): regras de score sobre o valor do Pix e a
// velocidade da conta (quantidade e soma na última hora), e o caso de uso Evaluate. Não depende
// de Redis, gRPC nem OpenTelemetry; tempo e contadores chegam por portas.
package risk

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Decision é o resultado da avaliação.
type Decision int

// Decisões.
const (
	Approve Decision = iota + 1
	Deny
)

func (d Decision) String() string {
	switch d {
	case Approve:
		return "approve"
	case Deny:
		return "deny"
	default:
		return "unknown"
	}
}

// Signal é um sinal de risco que contribuiu para o score.
type Signal int

// Sinais de risco.
const (
	SignalHighAmount Signal = iota + 1
	SignalHighVelocityCount
	SignalHighVelocityAmount
	// SignalVelocityUnavailable: contadores indisponíveis e valor acima do teto degradado.
	SignalVelocityUnavailable
)

// Request é o pedido de avaliação, já validado.
type Request struct {
	// TransferID é a chave de idempotência da avaliação: o mesmo Pix só conta uma vez.
	TransferID  uuid.UUID
	AccountID   uuid.UUID
	AmountCents int64
}

// ValidationError descreve um campo de pedido fora do formato aceito.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("risk: invalid %s: %s", e.Field, e.Problem)
}

// NewRequest valida os campos vindos de fora.
func NewRequest(transferID, accountID string, amountCents int64) (Request, error) {
	t, err := uuid.Parse(transferID)
	if err != nil {
		return Request{}, &ValidationError{Field: "transfer_id", Problem: "must be a UUID"}
	}
	a, err := uuid.Parse(accountID)
	if err != nil {
		return Request{}, &ValidationError{Field: "account_id", Problem: "must be a UUID"}
	}
	if amountCents <= 0 {
		return Request{}, &ValidationError{Field: "amount_cents", Problem: "must be positive"}
	}
	return Request{TransferID: t, AccountID: a, AmountCents: amountCents}, nil
}

// Assessment é o resultado da avaliação.
type Assessment struct {
	Decision Decision
	// Score de 0 a 100; quanto maior, mais arriscado.
	Score   int
	Signals []Signal
	// Degraded: os contadores não estavam disponíveis e a decisão usou só as regras sem
	// histórico (ADR-0008, seção 3).
	Degraded bool
}

// Rules são os limites das regras (ADR-0008, seção 1), configuráveis por ambiente.
type Rules struct {
	HighAmountCents     int64 // valor acima disto: +20
	VelocityCountMedium int64 // quantidade na última hora (contando este) a partir disto: +20
	VelocityCountHigh   int64 // ... a partir disto: +40
	VelocitySumMedium   int64 // soma na última hora (contando este) acima disto: +20
	VelocitySumHigh     int64 // ... acima disto: +40
	DenyScore           int   // recusa com score a partir disto
	DegradedMaxAmount   int64 // sem contadores, aprova até este valor
}

// DefaultRules são os valores do ADR-0008.
func DefaultRules() Rules {
	return Rules{
		HighAmountCents:     100_000,
		VelocityCountMedium: 5,
		VelocityCountHigh:   10,
		VelocitySumMedium:   200_000,
		VelocitySumHigh:     500_000,
		DenyScore:           60,
		DegradedMaxAmount:   50_000,
	}
}

// Validate confere a coerência dos limites (configuração errada impede o boot).
func (r Rules) Validate() error {
	var errs []error
	if r.HighAmountCents <= 0 || r.VelocitySumMedium <= 0 || r.DegradedMaxAmount <= 0 {
		errs = append(errs, errors.New("os limites de valor devem ser positivos"))
	}
	if r.VelocityCountMedium <= 0 || r.VelocityCountHigh < r.VelocityCountMedium {
		errs = append(errs, errors.New("limites de quantidade: 0 < médio <= alto"))
	}
	if r.VelocitySumHigh < r.VelocitySumMedium {
		errs = append(errs, errors.New("limites de soma: médio <= alto"))
	}
	if r.DenyScore < 1 || r.DenyScore > 100 {
		errs = append(errs, errors.New("o score de recusa deve estar entre 1 e 100"))
	}
	return errors.Join(errs...)
}

// Score aplica as regras sobre o valor e a velocidade estimada da conta.
func (r Rules) Score(amountCents int64, v Velocity) Assessment {
	var a Assessment
	if amountCents > r.HighAmountCents {
		a.Score += 20
		a.Signals = append(a.Signals, SignalHighAmount)
	}
	switch {
	case v.Count >= r.VelocityCountHigh:
		a.Score += 40
		a.Signals = append(a.Signals, SignalHighVelocityCount)
	case v.Count >= r.VelocityCountMedium:
		a.Score += 20
		a.Signals = append(a.Signals, SignalHighVelocityCount)
	}
	switch {
	case v.SumCents > r.VelocitySumHigh:
		a.Score += 40
		a.Signals = append(a.Signals, SignalHighVelocityAmount)
	case v.SumCents > r.VelocitySumMedium:
		a.Score += 20
		a.Signals = append(a.Signals, SignalHighVelocityAmount)
	}
	a.Score = min(a.Score, 100)
	a.Decision = Approve
	if a.Score >= r.DenyScore {
		a.Decision = Deny
	}
	return a
}

// Degraded decide sem histórico: aprova até o teto degradado, recusa acima dele.
func (r Rules) Degraded(amountCents int64) Assessment {
	a := Assessment{Decision: Approve, Degraded: true}
	if amountCents > r.HighAmountCents {
		a.Score += 20
		a.Signals = append(a.Signals, SignalHighAmount)
	}
	if amountCents > r.DegradedMaxAmount {
		a.Decision = Deny
		a.Score = 100
		a.Signals = append(a.Signals, SignalVelocityUnavailable)
	}
	return a
}
