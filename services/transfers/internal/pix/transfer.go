// Package pix é o domínio do transfers (ADR-0007): o Pix, a máquina de estados da saga, o
// limite diário e o aceite idempotente. Não depende de banco, gRPC nem OpenTelemetry; tempo,
// identificadores e persistência chegam por parâmetros e portas.
package pix

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	maxDescriptionRunes    = 140
	maxIdempotencyKeyBytes = 128
)

// Erros de domínio; a borda gRPC os traduz para status e ErrorInfo.
var (
	// ErrNotFound: o Pix não existe ou não é da conta informada.
	ErrNotFound = errors.New("pix: not found")
	// ErrIdempotencyKeyReused: a chave já foi usada, pela mesma conta, com outro pedido.
	ErrIdempotencyKeyReused = errors.New("pix: idempotency key reused with a different request")
	// ErrInvalidTransition: par (de, para) fora da máquina de estados. Bug, nunca entrada do cliente.
	ErrInvalidTransition = errors.New("pix: invalid state transition")
	// ErrLeaseLost: o lease da transferência passou para outro runner (fencing token, ADR-0007).
	ErrLeaseLost = errors.New("pix: lease lost")
	// ErrLimitExceeded: a reserva não cabe no limite diário.
	ErrLimitExceeded = errors.New("pix: daily limit exceeded")
)

// ValidationError descreve um campo de pedido fora do formato aceito.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("pix: invalid %s: %s", e.Field, e.Problem)
}

func invalid(field, problem string) error {
	return &ValidationError{Field: field, Problem: problem}
}

// IdempotencyKey é a chave vinda do cliente: de 1 a 128 caracteres ASCII visíveis.
type IdempotencyKey string

// NewIdempotencyKey valida a chave.
func NewIdempotencyKey(s string) (IdempotencyKey, error) {
	if s == "" || len(s) > maxIdempotencyKeyBytes {
		return "", invalid("idempotency_key", "must have 1 to 128 characters")
	}
	for i := range len(s) {
		if s[i] < '!' || s[i] > '~' {
			return "", invalid("idempotency_key", "must have only visible ASCII characters")
		}
	}
	return IdempotencyKey(s), nil
}

// NewDescription valida a descrição: opcional, até 140 caracteres, sem controle.
func NewDescription(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", invalid("description", "must be valid UTF-8")
	}
	v := strings.TrimSpace(s)
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", invalid("description", "must not contain control characters")
	}
	if utf8.RuneCountInString(v) > maxDescriptionRunes {
		return "", invalid("description", "is too long")
	}
	return v, nil
}

// ParseAccountID valida um ID de conta vindo de fora.
func ParseAccountID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.UUID{}, invalid(field, "must be a UUID")
	}
	return id, nil
}

// StartCommand é o pedido de Pix, já validado.
type StartCommand struct {
	Key            IdempotencyKey
	AccountID      uuid.UUID
	DestinationKey Key
	AmountCents    int64
	Description    string
	// Trace é o contexto de trace da requisição (carrier W3C), gravado para a saga continuar o
	// mesmo trace (ADR-0007, seção 4). Opaco para o domínio.
	Trace map[string]string
}

// NewStartCommand valida os campos do pedido.
func NewStartCommand(key, accountID string, keyType KeyType, keyValue string, amountCents int64, description string) (StartCommand, error) {
	k, err := NewIdempotencyKey(key)
	if err != nil {
		return StartCommand{}, err
	}
	acc, err := ParseAccountID("account_id", accountID)
	if err != nil {
		return StartCommand{}, err
	}
	dest, err := NewKey(keyType, keyValue)
	if err != nil {
		return StartCommand{}, err
	}
	if amountCents <= 0 {
		return StartCommand{}, invalid("amount_cents", "must be positive")
	}
	desc, err := NewDescription(description)
	if err != nil {
		return StartCommand{}, err
	}
	return StartCommand{Key: k, AccountID: acc, DestinationKey: dest, AmountCents: amountCents, Description: desc}, nil
}

// Transfer é o Pix com o estado da saga.
type Transfer struct {
	ID                   uuid.UUID
	SourceAccountID      uuid.UUID
	DestinationKey       Key
	DestinationAccountID uuid.UUID // zero até a validação do destino
	AmountCents          int64
	Description          string
	State                State
	Reason               Reason // zero quando não há
	// SettlementID é gerado no aceite e persistido antes de qualquer chamada ao SPI; todas
	// as tentativas usam o mesmo (seção 4.5 do plano).
	SettlementID uuid.UUID
	DebitEntryID uuid.UUID // lançamento do débito no ledger; zero antes do débito
	// LimitDay é o dia (America/Sao_Paulo) a que a reserva do limite pertence; a devolução vai
	// sempre para ele, nunca para o dia corrente (ADR-0007, seção 6).
	LimitDay      Day
	LimitReserved bool
	// SettlementMaybeSent: alguma tentativa de liquidação pode ter saído. Nunca volta a falso
	// (ADR-0007, seção 5).
	SettlementMaybeSent bool
	ConfirmationOverdue bool
	Trace               map[string]string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Change é uma linha do histórico de estados.
type Change struct {
	From   State // zero na criação
	To     State
	Reason Reason
	At     time.Time
}
