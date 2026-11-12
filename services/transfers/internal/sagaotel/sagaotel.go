// Package sagaotel é o adaptador de OpenTelemetry da saga: implementa saga.Tracing (cada etapa
// é um span filho do contexto gravado no aceite), saga.Observer e clients.Observer (métricas).
//
// Cardinalidade (seção 7.2 do plano): rótulos só de conjunto fixo (estados, motivos, etapas,
// dependências). O ID do Pix vai só para o span.
package sagaotel

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"conta.local/transfers/internal/clients"
	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

const scope = "conta.local/transfers/saga"

const (
	keyFrom         = attribute.Key("transfers.from")
	keyTo           = attribute.Key("transfers.to")
	keyState        = attribute.Key("transfers.state")
	keyReason       = attribute.Key("transfers.reason")
	keyStep         = attribute.Key("transfers.step")
	keyResult       = attribute.Key("transfers.result")
	keyCompensation = attribute.Key("transfers.compensation")
	keySettle       = attribute.Key("transfers.settle_result")
	keyDependency   = attribute.Key("dependency")
	keyPixID        = attribute.Key("transfers.pix_id")
	keyQuery        = attribute.Key("transfers.status_query_result")
)

// Telemetry implementa saga.Tracing, saga.Observer e clients.Observer.
type Telemetry struct {
	tracer      trace.Tracer
	propagator  propagation.TextMapPropagator
	transitions metric.Int64Counter
	outcomes    metric.Int64Counter
	compensated metric.Int64Counter
	stepTime    metric.Float64Histogram
	settle      metric.Int64Counter
	leaseLost   metric.Int64Counter
	retries     metric.Int64Counter
	queries     metric.Int64Counter
	overdue     metric.Int64Counter

	mu       sync.Mutex
	breakers map[string]resilience.State
}

var (
	_ saga.Tracing     = (*Telemetry)(nil)
	_ saga.Observer    = (*Telemetry)(nil)
	_ clients.Observer = (*Telemetry)(nil)
)

// New cria os instrumentos. dependencies são as dependências cujo circuito vira métrica (todas
// começam fechadas, para a série existir antes da primeira mudança).
func New(tp trace.TracerProvider, mp metric.MeterProvider, prop propagation.TextMapPropagator, dependencies ...string) (*Telemetry, error) {
	m := mp.Meter(scope)
	t := &Telemetry{tracer: tp.Tracer(scope), propagator: prop, breakers: map[string]resilience.State{}}
	for _, d := range dependencies {
		t.breakers[d] = resilience.Closed
	}
	var err error
	if t.transitions, err = m.Int64Counter("transfers.saga.transitions", metric.WithUnit("{transition}"),
		metric.WithDescription("Transições da saga, por estado de origem e de chegada.")); err != nil {
		return nil, err
	}
	if t.outcomes, err = m.Int64Counter("transfers.pix.outcomes", metric.WithUnit("{pix}"),
		metric.WithDescription("Pix que chegaram a um estado terminal, por estado e motivo.")); err != nil {
		return nil, err
	}
	if t.compensated, err = m.Int64Counter("transfers.compensations", metric.WithUnit("{compensation}"),
		metric.WithDescription("Compensações: devolução do limite e estorno do débito.")); err != nil {
		return nil, err
	}
	if t.stepTime, err = m.Float64Histogram("transfers.saga.step.duration", metric.WithUnit("s"),
		metric.WithDescription("Duração de cada etapa da saga, por etapa e resultado."),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5)); err != nil {
		return nil, err
	}
	if t.settle, err = m.Int64Counter("transfers.settlement.attempts", metric.WithUnit("{attempt}"),
		metric.WithDescription("Tentativas de liquidação: settled, rejected, not_sent (circuito aberto) e ambiguous (timeout e afins).")); err != nil {
		return nil, err
	}
	if t.leaseLost, err = m.Int64Counter("transfers.leases.lost", metric.WithUnit("{lease}"),
		metric.WithDescription("Leases perdidos para outro runner (fencing token barrou a escrita).")); err != nil {
		return nil, err
	}
	if t.retries, err = m.Int64Counter("transfers.dependency.retries", metric.WithUnit("{retry}"),
		metric.WithDescription("Novas tentativas de chamada, por dependência.")); err != nil {
		return nil, err
	}
	if t.queries, err = m.Int64Counter("transfers.confirmation.status_queries", metric.WithUnit("{query}"),
		metric.WithDescription("Consultas de status de Pix pendentes, por desfecho: settled, rejected, not_found, failed.")); err != nil {
		return nil, err
	}
	if t.overdue, err = m.Int64Counter("transfers.confirmation.overdue", metric.WithUnit("{pix}"),
		metric.WithDescription("Pix que passaram do prazo máximo de confirmação (continuam pendentes).")); err != nil {
		return nil, err
	}
	state, err := m.Int64ObservableGauge("transfers.circuit_breaker.state",
		metric.WithDescription("Estado do circuito por dependência: 0 fechado, 1 meio-aberto, 2 aberto."))
	if err != nil {
		return nil, err
	}
	if _, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		t.mu.Lock()
		defer t.mu.Unlock()
		for dep, s := range t.breakers {
			o.ObserveInt64(state, int64(s), metric.WithAttributes(keyDependency.String(dep)))
		}
		return nil
	}, state); err != nil {
		return nil, err
	}
	return t, nil
}

// Start abre o span da etapa como filho do contexto gravado no aceite: o Pix tem um trace só,
// do BFF até a última etapa, mesmo que a saga seja retomada por outra instância.
func (t *Telemetry) Start(ctx context.Context, carrier map[string]string, name, pixID string) (context.Context, func(error)) {
	parent := t.propagator.Extract(ctx, propagation.MapCarrier(carrier))
	sctx, span := t.tracer.Start(parent, name, trace.WithAttributes(keyPixID.String(pixID)))
	return sctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(otelcodes.Error, err.Error())
		}
		span.End()
	}
}

// Transitioned conta a transição e, se terminal, o desfecho.
func (t *Telemetry) Transitioned(ctx context.Context, from, to pix.State, reason pix.Reason) {
	t.transitions.Add(ctx, 1, metric.WithAttributes(keyFrom.String(from.String()), keyTo.String(to.String())))
	trace.SpanFromContext(ctx).AddEvent("pix.transition", trace.WithAttributes(
		keyFrom.String(from.String()), keyTo.String(to.String()), keyReason.String(reason.String())))
	if to.Terminal() {
		t.outcomes.Add(ctx, 1, metric.WithAttributes(keyState.String(to.String()), keyReason.String(reason.String())))
	}
}

// StepFinished registra a duração da etapa.
func (t *Telemetry) StepFinished(ctx context.Context, step pix.State, result string, d time.Duration) {
	t.stepTime.Record(ctx, d.Seconds(), metric.WithAttributes(keyStep.String(step.String()), keyResult.String(result)))
}

// Compensated conta uma compensação.
func (t *Telemetry) Compensated(ctx context.Context, kind string) {
	t.compensated.Add(ctx, 1, metric.WithAttributes(keyCompensation.String(kind)))
}

// SettleAttempt conta uma tentativa de liquidação pelo desfecho.
func (t *Telemetry) SettleAttempt(ctx context.Context, result string) {
	t.settle.Add(ctx, 1, metric.WithAttributes(keySettle.String(result)))
}

// StatusQueried conta uma consulta de status pelo desfecho.
func (t *Telemetry) StatusQueried(ctx context.Context, result string) {
	t.queries.Add(ctx, 1, metric.WithAttributes(keyQuery.String(result)))
}

// ConfirmationOverdue conta um Pix que passou do prazo máximo de confirmação.
func (t *Telemetry) ConfirmationOverdue(ctx context.Context) { t.overdue.Add(ctx, 1) }

// RegisterPending expõe, a cada coleta, os Pix pendentes de confirmação, quantos passaram do
// prazo e a idade do mais antigo (seção 4.5 do plano; base do alerta da Fase 6). Os números vêm
// do banco: valem para todas as instâncias, não só para esta.
func (t *Telemetry) RegisterPending(mp metric.MeterProvider, stats func(ctx context.Context) (saga.PendingStats, error)) error {
	m := mp.Meter(scope)
	pendingG, err := m.Int64ObservableGauge("transfers.confirmation.pending", metric.WithUnit("{pix}"),
		metric.WithDescription("Pix em PENDING_CONFIRMATION agora."))
	if err != nil {
		return err
	}
	overdueG, err := m.Int64ObservableGauge("transfers.confirmation.pending_overdue", metric.WithUnit("{pix}"),
		metric.WithDescription("Pix pendentes além do prazo máximo de confirmação, esperando a conciliação."))
	if err != nil {
		return err
	}
	oldestG, err := m.Float64ObservableGauge("transfers.confirmation.oldest_pending_age", metric.WithUnit("s"),
		metric.WithDescription("Há quanto tempo o Pix pendente mais antigo espera confirmação."))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		s, err := stats(ctx)
		if err != nil {
			return err
		}
		o.ObserveInt64(pendingG, s.Pending)
		o.ObserveInt64(overdueG, s.Overdue)
		o.ObserveFloat64(oldestG, s.OldestAge.Seconds())
		return nil
	}, pendingG, overdueG, oldestG)
	return err
}

// LeaseLost conta um lease perdido.
func (t *Telemetry) LeaseLost(ctx context.Context) { t.leaseLost.Add(ctx, 1) }

// Retried conta uma nova tentativa de chamada.
func (t *Telemetry) Retried(dependency string) {
	t.retries.Add(context.Background(), 1, metric.WithAttributes(keyDependency.String(dependency)))
}

// BreakerState guarda o estado do circuito, lido pelo gauge.
func (t *Telemetry) BreakerState(dependency string, s resilience.State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.breakers[dependency] = s
}
