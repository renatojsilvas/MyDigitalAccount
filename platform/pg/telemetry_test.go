package pg_test

import (
	"context"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"conta.local/platform/pg"
	"conta.local/testkit/testpg"
)

// Spans das queries como filhos da chamada, sem os valores dos parâmetros (que podem ser um
// CPF); métricas do pool exportadas; a sonda de prontidão não gera spans.
func TestPoolTelemetry(t *testing.T) {
	db := testpg.New(t)
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	cfg := fromTestDB(db)
	cfg.MaxConns = 3
	pool, err := pg.Open(t.Context(), cfg, pg.WithTelemetry(tp, mp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	spans.Reset() // conexões abertas no boot

	if err := pg.Ping(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if n := len(spans.GetSpans()); n != 0 {
		t.Fatalf("Ping gerou %d spans; a sonda de prontidão não deve gerar traces", n)
	}

	ctx, parent := tp.Tracer("t").Start(context.Background(), "rpc")
	var echoed string
	const cpf = "11144477735"
	if err := pool.QueryRow(ctx, "SELECT $1::text", cpf).Scan(&echoed); err != nil {
		t.Fatal(err)
	}
	parent.End()

	var query *tracetest.SpanStub
	for i, s := range spans.GetSpans() {
		if s.Name != "rpc" {
			query = &spans.GetSpans()[i]
		}
	}
	if query == nil {
		t.Fatal("nenhum span de query")
	}
	if query.Parent.SpanID() != parent.SpanContext().SpanID() {
		t.Error("o span da query deve ser filho do span da chamada")
	}
	var sawStatement bool
	for _, kv := range query.Attributes {
		if strings.Contains(kv.Value.String(), cpf) {
			t.Errorf("o atributo %s contém o valor do parâmetro (CPF): %q", kv.Key, kv.Value.String())
		}
		if kv.Key == "db.query.text" {
			sawStatement = true
		}
	}
	if !sawStatement {
		t.Errorf("o span da query deveria ter db.query.text; atributos: %v", query.Attributes)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	for _, want := range []string{"pgxpool.max_connections", "pgxpool.acquired_connections", "pgxpool.empty_acquire_wait_time", "db.client.operation.duration"} {
		if !names[want] {
			t.Errorf("métrica %s ausente; exportadas: %v", want, names)
		}
	}
}
