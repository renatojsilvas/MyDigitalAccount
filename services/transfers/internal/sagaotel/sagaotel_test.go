package sagaotel_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/sagaotel"
)

// A etapa da saga continua o trace gravado no aceite (critério de pronto: um trace só).
func TestStepSpanContinuesTheAcceptTrace(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	prop := propagation.TraceContext{}
	tel, err := sagaotel.New(tp, sdkmetric.NewMeterProvider(), prop)
	if err != nil {
		t.Fatal(err)
	}

	acceptCtx, accept := tp.Tracer("t").Start(context.Background(), "StartPix")
	carrier := propagation.MapCarrier{}
	prop.Inject(acceptCtx, carrier)
	accept.End()

	// A saga roda depois, sem o contexto da requisição.
	_, end := tel.Start(context.Background(), carrier, "pix.saga.accepted", "pix-1")
	end(errors.New("antifraude fora do ar"))

	got := spans.GetSpans()
	step := got[len(got)-1]
	if step.SpanContext.TraceID() != accept.SpanContext().TraceID() || step.Parent.SpanID() != accept.SpanContext().SpanID() {
		t.Fatal("o span da etapa não é filho do span do aceite")
	}
	if step.Status.Code.String() != "Error" || len(step.Events) == 0 {
		t.Fatalf("erro não registrado no span: %+v", step.Status)
	}
}

func TestMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	tel, err := sagaotel.New(sdktrace.NewTracerProvider(), sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		propagation.TraceContext{}, "antifraude", "spi")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tel.Transitioned(ctx, pix.StateSettling, pix.StateReversing, pix.ReasonSettlementRejected)
	tel.Transitioned(ctx, pix.StateReversing, pix.StateReversed, pix.ReasonSettlementRejected)
	tel.Compensated(ctx, "reversal")
	tel.SettleAttempt(ctx, "ambiguous")
	tel.Retried("antifraude")
	tel.BreakerState("spi", resilience.Open)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	breaker := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					seen[m.Name] += int(p.Value)
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					dep, _ := p.Attributes.Value("dependency")
					breaker[dep.AsString()] = p.Value
				}
			}
		}
	}
	want := map[string]int{
		"transfers.saga.transitions": 2, "transfers.pix.outcomes": 1, "transfers.compensations": 1,
		"transfers.settlement.attempts": 1, "transfers.dependency.retries": 1,
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %d, want %d", k, seen[k], v)
		}
	}
	if breaker["spi"] != 2 || breaker["antifraude"] != 0 {
		t.Fatalf("estado dos circuitos = %v, want spi aberto (2) e antifraude fechado (0)", breaker)
	}
}
