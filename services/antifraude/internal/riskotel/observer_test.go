package riskotel_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"conta.local/antifraude/internal/faults"
	"conta.local/antifraude/internal/risk"
	"conta.local/antifraude/internal/riskotel"
)

type point struct {
	attrs attribute.Set
	value float64
}

func collect(t *testing.T, r *sdkmetric.ManualReader) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, float64(p.Value)})
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, p.Value})
				}
			case metricdata.Histogram[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, float64(p.Count)})
				}
			}
		}
	}
	return out
}

func label(p point, k string) string {
	v, _ := p.attrs.Value(attribute.Key(k))
	return v.String()
}

func TestEvaluatedDegraded(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	inj := faults.New(faults.Config{Latency: 250 * time.Millisecond, ErrorRate: 0.2})
	obs, err := riskotel.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), inj)
	if err != nil {
		t.Fatal(err)
	}
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	ctx, span := tp.Tracer("t").Start(context.Background(), "Evaluate")

	req, _ := risk.NewRequest(uuid.NewString(), uuid.NewString(), 60_000)
	obs.Evaluated(ctx, req, risk.DefaultRules().Degraded(60_000), risk.DegradedTimeout)
	span.End()

	m := collect(t, reader)
	d := m["antifraude.decisions"]
	if len(d) != 1 || d[0].value != 1 || label(d[0], "antifraude.decision") != "deny" || label(d[0], "antifraude.degraded") != "true" {
		t.Fatalf("decisões = %+v", d)
	}
	if g := m["antifraude.degraded"]; len(g) != 1 || label(g[0], "antifraude.degraded_reason") != "timeout" {
		t.Fatalf("degradadas = %+v", g)
	}
	if h := m["antifraude.score"]; len(h) != 1 || h[0].value != 1 {
		t.Fatalf("score = %+v", h)
	}
	if l := m["antifraude.fault.latency"]; len(l) != 1 || l[0].value != 0.25 {
		t.Fatalf("latência injetada = %+v", l)
	}
	if r := m["antifraude.fault.error_rate"]; len(r) != 1 || r[0].value != 0.2 {
		t.Fatalf("taxa de erro injetada = %+v", r)
	}
	// Cardinalidade: IDs nunca viram rótulo.
	for name, ps := range m {
		for _, p := range ps {
			if p.attrs.HasValue("antifraude.account_id") || p.attrs.HasValue("antifraude.transfer_id") {
				t.Errorf("%s tem ID como rótulo", name)
			}
		}
	}

	got := map[string]string{}
	for _, kv := range spans.GetSpans()[0].Attributes {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got["antifraude.degraded"] != "true" || got["antifraude.degraded_reason"] != "timeout" ||
		got["antifraude.account_id"] != req.AccountID.String() || got["antifraude.score"] != "100" {
		t.Fatalf("atributos do span = %v", got)
	}
}
