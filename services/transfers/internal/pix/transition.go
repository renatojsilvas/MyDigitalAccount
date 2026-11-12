package pix

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Transition é uma mudança de estado da saga, com os dados que ela grava.
type Transition struct {
	From, To State
	Reason   Reason
	// Preenchidos na transição em que passam a existir; zero mantém o valor gravado.
	DestinationAccountID uuid.UUID
	DebitEntryID         uuid.UUID
	// NextRunAt é quando o runner deve olhar a transferência de novo.
	NextRunAt time.Time
	At        time.Time
}

// Validate confere a transição contra a máquina de estados e a coerência do motivo: recusas
// levam a REJECTED, estornos a REVERSING, e REVERSED mantém o motivo do estorno.
func (t Transition) Validate() error {
	if !CanTransition(t.From, t.To) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.From, t.To)
	}
	switch t.To {
	case StateRejected:
		if !t.Reason.IsRejection() {
			return fmt.Errorf("%w: rejected exige motivo de recusa, veio %q", ErrInvalidTransition, t.Reason)
		}
	case StateReversing, StateReversed:
		if !t.Reason.IsReversal() {
			return fmt.Errorf("%w: %s exige motivo de estorno, veio %q", ErrInvalidTransition, t.To, t.Reason)
		}
	default:
		if t.Reason != 0 {
			return fmt.Errorf("%w: %s não tem motivo", ErrInvalidTransition, t.To)
		}
	}
	if t.To == StateValidated && t.DestinationAccountID == (uuid.UUID{}) {
		return fmt.Errorf("%w: validated exige a conta de destino", ErrInvalidTransition)
	}
	if t.To == StateDebited && t.DebitEntryID == (uuid.UUID{}) {
		return fmt.Errorf("%w: debited exige o lançamento do débito", ErrInvalidTransition)
	}
	return nil
}

// ReleasesLimit diz se o estado de chegada devolve o limite reservado: o dinheiro não saiu
// (recusa) ou voltou (estorno).
func (t Transition) ReleasesLimit() bool {
	return t.To == StateRejected || t.To == StateReversed
}
