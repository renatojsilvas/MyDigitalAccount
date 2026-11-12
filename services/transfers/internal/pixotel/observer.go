// Package pixotel é o adaptador de OpenTelemetry da porta pix.Observer: aceites, repetições e
// conflitos de idempotência viram métricas de negócio e atributos do span da chamada.
//
// Cardinalidade (seção 7.2 do plano): rótulos só de conjunto fixo. IDs de Pix e de conta vão
// só para o span; a chave Pix de destino vai mascarada, e só para o span.
package pixotel

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"conta.local/transfers/internal/pix"
)

const meterName = "conta.local/transfers/pix"

const (
	keyPixID          = attribute.Key("transfers.pix_id")
	keyAccountID      = attribute.Key("transfers.account_id")
	keyDestinationKey = attribute.Key("transfers.destination_key")
	keyReplay         = attribute.Key("transfers.replay")
	keyState          = attribute.Key("transfers.state")
)

// Observer implementa pix.Observer.
type Observer struct {
	started   metric.Int64Counter
	amount    metric.Int64Counter
	conflicts metric.Int64Counter
}

var _ pix.Observer = (*Observer)(nil)

// New cria os instrumentos.
func New(mp metric.MeterProvider) (*Observer, error) {
	m := mp.Meter(meterName)
	var o Observer
	var err error
	if o.started, err = m.Int64Counter("transfers.pix.started", metric.WithUnit("{pix}"),
		metric.WithDescription("Pedidos de Pix aceitos; replay = repetição pela mesma chave (não cria Pix).")); err != nil {
		return nil, err
	}
	if o.amount, err = m.Int64Counter("transfers.pix.started_amount", metric.WithUnit("{cent}"),
		metric.WithDescription("Valor dos Pix aceitos (novos), em centavos.")); err != nil {
		return nil, err
	}
	if o.conflicts, err = m.Int64Counter("transfers.idempotency.conflicts", metric.WithUnit("{request}"),
		metric.WithDescription("Chave de idempotência reutilizada com outro pedido.")); err != nil {
		return nil, err
	}
	return &o, nil
}

// Started registra um aceite (novo ou repetido).
func (o *Observer) Started(ctx context.Context, t pix.Transfer, created bool) {
	trace.SpanFromContext(ctx).SetAttributes(
		keyPixID.String(t.ID.String()), keyAccountID.String(t.SourceAccountID.String()),
		keyDestinationKey.String(t.DestinationKey.Masked()), keyReplay.Bool(!created), keyState.String(t.State.String()),
	)
	o.started.Add(ctx, 1, metric.WithAttributes(keyReplay.String(strconv.FormatBool(!created))))
	if created {
		o.amount.Add(ctx, t.AmountCents)
	}
}

// KeyConflict registra uma chave reutilizada com outro pedido.
func (o *Observer) KeyConflict(ctx context.Context) {
	o.conflicts.Add(ctx, 1)
}
