package pix

import "fmt"

// State é o estado da saga do Pix (ADR-0007, seção 2).
type State int

// Estados.
const (
	StateAccepted State = iota + 1
	StateValidated
	StateDebited
	StateSettling
	StatePendingConfirmation
	StateSettled
	StateCompleted
	StateReversing
	StateReversed
	StateRejected
)

var stateNames = map[State]string{
	StateAccepted: "accepted", StateValidated: "validated", StateDebited: "debited", StateSettling: "settling",
	StatePendingConfirmation: "pending_confirmation", StateSettled: "settled", StateCompleted: "completed",
	StateReversing: "reversing", StateReversed: "reversed", StateRejected: "rejected",
}

// States lista os estados, em ordem.
func States() []State {
	return []State{StateAccepted, StateValidated, StateDebited, StateSettling, StatePendingConfirmation,
		StateSettled, StateCompleted, StateReversing, StateReversed, StateRejected}
}

func (s State) String() string {
	if n, ok := stateNames[s]; ok {
		return n
	}
	return "unknown"
}

// ParseState lê o nome de um estado (o mesmo de String).
func ParseState(name string) (State, error) {
	for s, n := range stateNames {
		if n == name {
			return s, nil
		}
	}
	return 0, fmt.Errorf("pix: estado desconhecido %q", name)
}

// Terminal diz se o estado encerra a saga.
func (s State) Terminal() bool {
	return s == StateCompleted || s == StateReversed || s == StateRejected
}

// transitions é a máquina de estados: os únicos pares (de, para) permitidos. Qualquer outro é
// bug de quem chama, e Transition falha (ADR-0007, seção 2).
var transitions = map[State][]State{
	StateAccepted: {StateValidated, StateRejected},
	// Recusa depois da validação: saldo insuficiente no débito.
	StateValidated: {StateDebited, StateRejected},
	StateDebited:   {StateSettling},
	StateSettling: {
		StateSettled,             // liquidado
		StateReversing,           // recusa explícita, ou "não enviado" com prova
		StatePendingConfirmation, // ambíguo
	},
	StatePendingConfirmation: {StateSettled, StateReversing},
	StateSettled:             {StateCompleted},
	StateReversing:           {StateReversed},
}

// CanTransition diz se a transição é permitida.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Status é a situação do Pix como o cliente a vê.
type Status int

// Situações.
const (
	StatusProcessing Status = iota + 1
	StatusCompleted
	StatusFailed
	StatusRejected
)

func (s Status) String() string {
	switch s {
	case StatusProcessing:
		return "processing"
	case StatusCompleted:
		return "completed"
	case StatusFailed:
		return "failed"
	case StatusRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Status deriva a visão do cliente do estado. Regra do transfers, não do BFF (regra 1):
// qualquer estado não terminal, inclusive pendente de confirmação, é "em processamento".
func (s State) Status() Status {
	switch s {
	case StateCompleted:
		return StatusCompleted
	case StateReversed:
		return StatusFailed
	case StateRejected:
		return StatusRejected
	default:
		return StatusProcessing
	}
}

// Reason é o motivo de uma recusa ou de um estorno.
type Reason int

// Motivos.
const (
	ReasonAntifraudDenied Reason = iota + 1
	ReasonLimitExceeded
	ReasonDestinationInvalid
	ReasonSameAccount
	ReasonInsufficientFunds
	ReasonDependencyUnavailable
	ReasonSettlementRejected
	ReasonSettlementNotSent
)

var reasonNames = map[Reason]string{
	ReasonAntifraudDenied: "antifraud_denied", ReasonLimitExceeded: "limit_exceeded",
	ReasonDestinationInvalid: "destination_invalid", ReasonSameAccount: "same_account",
	ReasonInsufficientFunds: "insufficient_funds", ReasonDependencyUnavailable: "dependency_unavailable",
	ReasonSettlementRejected: "settlement_rejected", ReasonSettlementNotSent: "settlement_not_sent",
}

func (r Reason) String() string {
	if n, ok := reasonNames[r]; ok {
		return n
	}
	return ""
}

// ParseReason lê o nome de um motivo; vazio é "sem motivo".
func ParseReason(name string) (Reason, error) {
	if name == "" {
		return 0, nil
	}
	for r, n := range reasonNames {
		if n == name {
			return r, nil
		}
	}
	return 0, fmt.Errorf("pix: motivo desconhecido %q", name)
}

// IsRejection diz se o motivo leva a REJECTED (antes de mover dinheiro).
func (r Reason) IsRejection() bool {
	return r >= ReasonAntifraudDenied && r <= ReasonDependencyUnavailable
}

// IsReversal diz se o motivo leva a REVERSING (estorno do débito).
func (r Reason) IsReversal() bool {
	return r == ReasonSettlementRejected || r == ReasonSettlementNotSent
}
