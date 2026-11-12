// Package riskotel é o adaptador de OpenTelemetry da porta risk.Observer: decisões, score e
// degradação viram métricas de negócio e atributos do span da chamada; as falhas injetadas
// viram métricas observáveis (ADR-0008).
//
// Cardinalidade (seção 7.2 do plano): rótulos só com conjunto fixo (decisão, degradado,
// motivo). Os IDs de conta e de Pix vão só para o span.
package riskotel

import (
	"context"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"conta.local/antifraude/internal/faults"
	"conta.local/antifraude/internal/risk"
)

const meterName = "conta.local/antifraude/risk"

const (
	keyDecision       = attribute.Key("antifraude.decision")
	keyDegraded       = attribute.Key("antifraude.degraded")
	keyDegradedReason = attribute.Key("antifraude.degraded_reason")
	keyScore          = attribute.Key("antifraude.score")
	keyAccountID      = attribute.Key("antifraude.account_id")
	keyTransferID     = attribute.Key("antifraude.transfer_id")
)

// Observer implementa risk.Observer.
type Observer struct {
	decisions metric.Int64Counter
	degraded  metric.Int64Counter
	score     metric.Int64Histogram
}

var _ risk.Observer = (*Observer)(nil)

// New cria os instrumentos. inj alimenta as métricas das falhas injetadas: assim, um pico de
// latência no transfers pode ser explicado no mesmo dashboard.
func New(mp metric.MeterProvider, inj *faults.Injector) (*Observer, error) {
	m := mp.Meter(meterName)
	var o Observer
	var err error
	if o.decisions, err = m.Int64Counter("antifraude.decisions",
		metric.WithUnit("{evaluation}"),
		metric.WithDescription("Avaliações, por decisão e por degradação.")); err != nil {
		return nil, err
	}
	if o.degraded, err = m.Int64Counter("antifraude.degraded",
		metric.WithUnit("{evaluation}"),
		metric.WithDescription("Avaliações sem os contadores do Redis, por motivo (timeout, error).")); err != nil {
		return nil, err
	}
	if o.score, err = m.Int64Histogram("antifraude.score",
		metric.WithUnit("1"),
		metric.WithDescription("Score das avaliações (0 a 100)."),
		metric.WithExplicitBucketBoundaries(0, 20, 40, 60, 80, 100)); err != nil {
		return nil, err
	}

	latency, err := m.Float64ObservableGauge("antifraude.fault.latency",
		metric.WithUnit("s"), metric.WithDescription("Latência injetada em cada Evaluate."))
	if err != nil {
		return nil, err
	}
	errorRate, err := m.Float64ObservableGauge("antifraude.fault.error_rate",
		metric.WithUnit("1"), metric.WithDescription("Fração de Evaluate que falham por injeção (0 a 1)."))
	if err != nil {
		return nil, err
	}
	if _, err := m.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		cfg := inj.Current()
		obs.ObserveFloat64(latency, cfg.Latency.Seconds())
		obs.ObserveFloat64(errorRate, cfg.ErrorRate)
		return nil
	}, latency, errorRate); err != nil {
		return nil, err
	}
	return &o, nil
}

// Evaluated registra a avaliação nas métricas e no span corrente.
func (o *Observer) Evaluated(ctx context.Context, req risk.Request, a risk.Assessment, reason risk.DegradedReason) {
	degraded := strconv.FormatBool(a.Degraded)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		keyDecision.String(a.Decision.String()),
		keyScore.Int(a.Score),
		keyDegraded.Bool(a.Degraded),
		keyAccountID.String(req.AccountID.String()),
		keyTransferID.String(req.TransferID.String()),
	)

	o.decisions.Add(ctx, 1, metric.WithAttributes(keyDecision.String(a.Decision.String()), keyDegraded.String(degraded)))
	o.score.Record(ctx, int64(a.Score), metric.WithAttributes(keyDegraded.String(degraded)))
	if a.Degraded {
		span.SetAttributes(keyDegradedReason.String(string(reason)))
		span.AddEvent("antifraude.degraded")
		o.degraded.Add(ctx, 1, metric.WithAttributes(keyDegradedReason.String(string(reason))))
	}
}
