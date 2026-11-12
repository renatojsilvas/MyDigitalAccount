package clients

import (
	"context"
	"errors"
	"fmt"

	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

// SPI implementa saga.SPI: uma tentativa por chamada, sem retry aqui (as tentativas, com o
// mesmo settlement_id, são da saga, que precisa gravar o marcador antes de cada uma).
type SPI struct {
	c  spiv1.SpiServiceClient
	br *resilience.Breaker
}

var _ saga.SPI = (*SPI)(nil)

// NewSPI cria o adaptador; só o circuito da dependência é usado.
func NewSPI(c spiv1.SpiServiceClient, dep *Dependency) *SPI {
	return &SPI{c: c, br: dep.Breaker}
}

// Settle faz uma tentativa (ADR-0007, seção 5): pede autorização ao circuito; autorizado, roda
// beforeSend (o marcador settlement_maybe_sent) e só então chama o SPI. Se o circuito recusar,
// ou se o marcador não puder ser gravado, nada sai, e o erro é resilience.ErrNotSent.
func (s *SPI) Settle(ctx context.Context, req saga.SettleRequest, beforeSend func() error) (saga.SettleStatus, error) {
	done, err := s.br.Allow()
	if err != nil {
		return 0, err
	}
	if err := beforeSend(); err != nil {
		// Nada saiu: não é sinal sobre o SPI, então não conta no circuito.
		done(context.Canceled)
		return 0, fmt.Errorf("%w: marcador não gravado: %w", resilience.ErrNotSent, err)
	}
	resp, err := s.c.Settle(ctx, &spiv1.SettleRequest{
		SettlementId: req.SettlementID.String(), TransferId: req.TransferID.String(),
		PayerAccountId: req.Payer.String(), PayeeAccountId: req.Payee.String(), AmountCents: req.AmountCents,
	})
	done(err)
	if err != nil {
		return 0, err // ambíguo: pode ter liquidado
	}
	switch resp.GetSettlement().GetStatus() {
	case spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED:
		return saga.Settled, nil
	case spiv1.SettlementStatus_SETTLEMENT_STATUS_REJECTED:
		return saga.Rejected, nil
	default:
		return 0, errors.New("clients: SPI respondeu sem status definitivo")
	}
}
