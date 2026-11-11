package telemetry_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"conta.local/ledger/internal/platform/telemetry"
)

// memLogExporter guarda os registros de log exportados.
type memLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *memLogExporter) Export(_ context.Context, rs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rs {
		e.records = append(e.records, r.Clone())
	}
	return nil
}
func (*memLogExporter) Shutdown(context.Context) error   { return nil }
func (*memLogExporter) ForceFlush(context.Context) error { return nil }

func TestNewExportsAllThreeSignals(t *testing.T) {
	ctx := t.Context()
	spans := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader()
	logs := &memLogExporter{}

	tel, err := telemetry.New(ctx,
		telemetry.Config{ServiceName: "ledger", Version: "test", Environment: "test", InstanceID: "replica-1"},
		telemetry.WithSpanExporter(spans), telemetry.WithMetricReader(reader), telemetry.WithLogExporter(logs))
	if err != nil {
		t.Fatal(err)
	}

	// Trace, com o atributo de recurso service.name.
	spanCtx, span := tel.Tracer.Tracer("test").Start(ctx, "op")
	// Log emitido dentro do span: o bridge deve carregar o mesmo trace.
	slog.New(tel.LogHandler("test")).InfoContext(spanCtx, "olá")
	span.End()

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans exportados = %d, esperado 1", len(got))
	}
	attrs := map[string]string{}
	for _, a := range got[0].Resource.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["service.name"] != "ledger" {
		t.Errorf("service.name = %q", attrs["service.name"])
	}
	if attrs["service.instance.id"] != "replica-1" {
		t.Errorf("service.instance.id = %q; réplicas precisam se distinguir", attrs["service.instance.id"])
	}

	logs.mu.Lock()
	if len(logs.records) != 1 {
		t.Fatalf("logs exportados = %d, esperado 1", len(logs.records))
	}
	if logs.records[0].TraceID() != got[0].SpanContext.TraceID() {
		t.Errorf("log sem correlação com o trace: %s != %s", logs.records[0].TraceID(), got[0].SpanContext.TraceID())
	}
	logs.mu.Unlock()

	// Métricas de runtime (método USE) registradas por padrão.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	if len(rm.ScopeMetrics) == 0 {
		t.Error("nenhuma métrica coletada")
	}

	if err := tel.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestNewWithoutEndpointDoesNotExport(t *testing.T) {
	tel, err := telemetry.New(t.Context(), telemetry.Config{ServiceName: "ledger"})
	if err != nil {
		t.Fatalf("sem endpoint a telemetria não deve falhar: %v", err)
	}
	if tel.Propagator == nil || tel.Tracer == nil || tel.Meter == nil || tel.Logger == nil {
		t.Error("providers não foram criados")
	}
	if err := tel.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
