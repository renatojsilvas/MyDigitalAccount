// Package settlement é o domínio do spi-fake, o simulador do SPI: liquidações idempotentes
// pelo settlement_id e um comportamento configurável para testar o timeout na liquidação
// (seção 4.5 do plano, ADR-0007). Não depende de banco nem de gRPC.
package settlement

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status é o resultado de uma liquidação.
type Status int

// Resultados.
const (
	Settled Status = iota + 1
	Rejected
)

func (s Status) String() string {
	switch s {
	case Settled:
		return "settled"
	case Rejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// RejectReason é o motivo de uma recusa.
type RejectReason int

// Motivos de recusa.
const (
	// RejectSimulated é a recusa produzida pelo modo Reject.
	RejectSimulated RejectReason = iota + 1
)

// Erros de domínio; a borda gRPC os traduz para status e ErrorInfo.
var (
	// ErrNotFound: não há liquidação registrada com esse ID (ainda).
	ErrNotFound = errors.New("settlement: not found")
	// ErrIDReused: o settlement_id já foi usado com um pedido diferente.
	ErrIDReused = errors.New("settlement: id reused with a different request")
	// ErrStatusUnavailable: falha simulada da consulta de status (FailStatusQueries).
	ErrStatusUnavailable = errors.New("settlement: status query failure (simulated)")
	// ErrNoResponse: o modo "sem responder" segurou a chamada até o teto, sem o cliente desistir.
	ErrNoResponse = errors.New("settlement: no response (simulated)")
)

// ValidationError descreve um campo de pedido fora do formato aceito.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("settlement: invalid %s: %s", e.Field, e.Problem)
}

// Request é o pedido de liquidação, já validado.
type Request struct {
	ID          uuid.UUID
	TransferID  uuid.UUID
	Payer       uuid.UUID
	Payee       uuid.UUID
	AmountCents int64
}

// NewRequest valida os campos vindos de fora.
func NewRequest(id, transferID, payer, payee string, amountCents int64) (Request, error) {
	var r Request
	for _, f := range []struct {
		name, value string
		dst         *uuid.UUID
	}{
		{"settlement_id", id, &r.ID}, {"transfer_id", transferID, &r.TransferID},
		{"payer_account_id", payer, &r.Payer}, {"payee_account_id", payee, &r.Payee},
	} {
		u, err := uuid.Parse(f.value)
		if err != nil {
			return Request{}, &ValidationError{Field: f.name, Problem: "must be a UUID"}
		}
		*f.dst = u
	}
	if amountCents <= 0 {
		return Request{}, &ValidationError{Field: "amount_cents", Problem: "must be positive"}
	}
	r.AmountCents = amountCents
	return r, nil
}

// ParseID valida o settlement_id de uma consulta.
func ParseID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, &ValidationError{Field: "settlement_id", Problem: "must be a UUID"}
	}
	return id, nil
}

// Settlement é uma liquidação registrada. O resultado nunca muda depois de registrado.
type Settlement struct {
	Request
	Status       Status
	RejectReason RejectReason // só quando Rejected
	DecidedAt    time.Time
}

// Matches diz se o pedido é o mesmo que originou a liquidação (mesmo ID e mesmos dados).
func (s Settlement) Matches(r Request) bool {
	return s.Request == r
}
