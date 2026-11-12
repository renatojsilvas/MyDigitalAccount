// Package saga é o orquestrador do Pix (ADR-0007): o runner pega transferências do Postgres
// (fila com lease e fencing token) e as leva pela máquina de estados, chamando antifraude,
// ledger e SPI. As dependências entram por portas definidas aqui; os adaptadores gRPC
// (internal/clients) aplicam retry e circuit breaker.
package saga

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"conta.local/transfers/internal/pix"
)

// Job é uma transferência tomada pelo runner, com o seu lease.
type Job struct {
	Transfer pix.Transfer
	Lease    pix.Lease
}

// Store é a persistência que o runner usa. Toda escrita confere o fencing token.
type Store interface {
	// Claim toma até maxJobs transferências vencidas nos estados dados (FOR UPDATE SKIP LOCKED).
	Claim(ctx context.Context, owner string, ttl time.Duration, states []pix.State, maxJobs int) ([]Job, error)
	// Renew estende o lease (heartbeat). pix.ErrLeaseLost se outro runner o tomou.
	Renew(ctx context.Context, lease pix.Lease, ttl time.Duration) error
	// Release solta o lease e agenda a próxima rodada.
	Release(ctx context.Context, lease pix.Lease, nextRunAt time.Time) error
	ReserveLimit(ctx context.Context, lease pix.Lease, dailyLimitCents int64, at time.Time) error
	Transition(ctx context.Context, lease pix.Lease, tr pix.Transition) error
	// MarkMaybeSent grava settlement_maybe_sent = true, antes de a chamada ao SPI sair.
	MarkMaybeSent(ctx context.Context, lease pix.Lease, at time.Time) error
}

// Erros das dependências, já traduzidos pelos adaptadores.
var (
	// ErrUnavailable: a dependência não respondeu depois das tentativas (ou o circuito está
	// aberto). Não diz nada sobre o efeito de uma escrita: só a chave de idempotência resolve.
	ErrUnavailable = errors.New("saga: dependency unavailable")
	// ErrAccountNotFound: o ledger não conhece a conta.
	ErrAccountNotFound = errors.New("saga: account not found")
	// ErrInsufficientFunds: o ledger recusou o débito por saldo.
	ErrInsufficientFunds = errors.New("saga: insufficient funds")
)

// Antifraud avalia o Pix. Idempotente por transferID.
type Antifraud interface {
	Evaluate(ctx context.Context, transferID, accountID uuid.UUID, amountCents int64) (approved bool, err error)
}

// Ledger é o dono do dinheiro. As escritas são idempotentes pela chave.
type Ledger interface {
	AccountByDocument(ctx context.Context, cpf string) (uuid.UUID, error)
	Debit(ctx context.Context, key string, accountID uuid.UUID, amountCents int64, reference, description string) (entryID uuid.UUID, err error)
	Credit(ctx context.Context, key string, accountID uuid.UUID, amountCents int64, reference, description string) error
	Reverse(ctx context.Context, key string, entryID uuid.UUID, reference, description string) error
}

// SettleRequest é o pedido de liquidação; SettlementID é o mesmo em todas as tentativas.
type SettleRequest struct {
	SettlementID, TransferID, Payer, Payee uuid.UUID
	AmountCents                            int64
}

// SettleStatus é a resposta definitiva do SPI.
type SettleStatus int

// Respostas definitivas.
const (
	Settled SettleStatus = iota + 1
	Rejected
)

// SPI liquida. Settle faz UMA tentativa: beforeSend roda depois de o circuito autorizar e antes
// de a chamada sair (é onde o marcador settlement_maybe_sent é gravado). Se o circuito recusar,
// o erro é resilience.ErrNotSent e beforeSend não roda: nada saiu. Qualquer outro erro é
// ambíguo (ADR-0007, seção 5).
type SPI interface {
	Settle(ctx context.Context, req SettleRequest, beforeSend func() error) (SettleStatus, error)
}

// Tracing abre o span de cada etapa como filho do contexto gravado no aceite, para o trace do
// Pix ser um só (ADR-0007, seção 4). Implementado sobre o OpenTelemetry fora deste pacote.
type Tracing interface {
	Start(ctx context.Context, carrier map[string]string, name, pixID string) (context.Context, func(err error))
}

// Observer recebe os eventos da saga, para métricas.
type Observer interface {
	Transitioned(ctx context.Context, from, to pix.State, reason pix.Reason)
	StepFinished(ctx context.Context, step pix.State, result string, d time.Duration)
	Compensated(ctx context.Context, kind string)
	SettleAttempt(ctx context.Context, result string)
	LeaseLost(ctx context.Context)
}
