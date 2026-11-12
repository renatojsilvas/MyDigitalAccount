// Package settlementotel é o adaptador de OpenTelemetry da porta settlement.Observer:
// liquidações, respostas seguradas e consultas de status viram métricas e atributos de trace;
// o comportamento em vigor vira métricas observáveis.
//
// Cardinalidade (seção 7.2 do plano): rótulos só de conjunto fixo (status, modo, desfecho).
// Os IDs de liquidação e de Pix vão só para o span.
package settlementotel

import (
	"context"
	"strconv"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"conta.local/spi-fake/internal/settlement"
)

const meterName = "conta.local/spi-fake/settlement"

const (
	keyStatus       = attribute.Key("spi.status")
	keyMode         = attribute.Key("spi.settle_mode")
	keyReplay       = attribute.Key("spi.replay")
	keyHeld         = attribute.Key("spi.held")
	keyQueryResult  = attribute.Key("spi.query_result")
	keySettlementID = attribute.Key("spi.settlement_id")
	keyTransferID   = attribute.Key("spi.transfer_id")
)

// Observer implementa settlement.Observer.
type Observer struct {
	settlements metric.Int64Counter
	held        metric.Int64Counter
	queries     metric.Int64Counter
}

var _ settlement.Observer = (*Observer)(nil)

// New cria os instrumentos. sw alimenta as métricas do comportamento em vigor: um pico de
// timeouts no transfers se explica no mesmo dashboard.
func New(mp metric.MeterProvider, sw *settlement.Switch) (*Observer, error) {
	m := mp.Meter(meterName)
	var o Observer
	var err error
	if o.settlements, err = m.Int64Counter("spi.settlements", metric.WithUnit("{settlement}"),
		metric.WithDescription("Settle respondidos ou segurados, por status registrado e repetição.")); err != nil {
		return nil, err
	}
	if o.held, err = m.Int64Counter("spi.settle.held", metric.WithUnit("{request}"),
		metric.WithDescription("Settle segurados sem resposta (modos sem responder), por modo.")); err != nil {
		return nil, err
	}
	if o.queries, err = m.Int64Counter("spi.status_queries", metric.WithUnit("{request}"),
		metric.WithDescription("Consultas de status, por desfecho (found, not_found, failed).")); err != nil {
		return nil, err
	}
	mode, err := m.Int64ObservableGauge("spi.behavior.settle_mode",
		metric.WithDescription("1 no modo de liquidação em vigor, 0 nos demais."))
	if err != nil {
		return nil, err
	}
	failQueries, err := m.Int64ObservableGauge("spi.behavior.fail_status_queries",
		metric.WithDescription("1 se as consultas de status estão falhando por simulação."))
	if err != nil {
		return nil, err
	}
	latency, err := m.Float64ObservableGauge("spi.behavior.latency", metric.WithUnit("s"),
		metric.WithDescription("Latência simulada antes de decidir cada Settle."))
	if err != nil {
		return nil, err
	}
	if _, err := m.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		b := sw.Current()
		for _, md := range settlement.Modes() {
			var v int64
			if md == b.Mode {
				v = 1
			}
			obs.ObserveInt64(mode, v, metric.WithAttributes(keyMode.String(md.String())))
		}
		var f int64
		if b.FailStatusQueries {
			f = 1
		}
		obs.ObserveInt64(failQueries, f)
		obs.ObserveFloat64(latency, b.Latency.Seconds())
		return nil
	}, mode, failQueries, latency); err != nil {
		return nil, err
	}
	return &o, nil
}

// Settled registra um Settle.
func (o *Observer) Settled(ctx context.Context, ev settlement.SettleEvent) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(keyMode.String(ev.Mode.String()), keyHeld.Bool(ev.Held), keyReplay.Bool(ev.Replay))
	if ev.Held {
		span.AddEvent("spi.response_held")
		o.held.Add(ctx, 1, metric.WithAttributes(keyMode.String(ev.Mode.String())))
	}
	span.SetAttributes(
		keySettlementID.String(ev.Settlement.ID.String()),
		keyTransferID.String(ev.Settlement.TransferID.String()),
	)
	if ev.Settlement.Status == 0 {
		return // não liquidou (NoSettleNoResponse)
	}
	span.SetAttributes(keyStatus.String(ev.Settlement.Status.String()))
	o.settlements.Add(ctx, 1, metric.WithAttributes(
		keyStatus.String(ev.Settlement.Status.String()), keyReplay.String(strconv.FormatBool(ev.Replay))))
}

// StatusQueried registra uma consulta de status.
func (o *Observer) StatusQueried(ctx context.Context, id uuid.UUID, result settlement.QueryResult) {
	trace.SpanFromContext(ctx).SetAttributes(keySettlementID.String(id.String()), keyQueryResult.String(string(result)))
	o.queries.Add(ctx, 1, metric.WithAttributes(keyQueryResult.String(string(result))))
}
