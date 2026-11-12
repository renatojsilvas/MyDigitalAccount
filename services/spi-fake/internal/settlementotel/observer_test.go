package settlementotel_test

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

	"conta.local/spi-fake/internal/settlement"
	"conta.local/spi-fake/internal/settlementotel"
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
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, float64(p.Value)})
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					out[m.Name] = append(out[m.Name], point{p.Attributes, p.Value})
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

func TestObserver(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	sw := settlement.NewSwitch(settlement.Behavior{
		Mode: settlement.ModeSettleNoResponse, FailStatusQueries: true, Latency: 500 * time.Millisecond,
	})
	obs, err := settlementotel.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), sw)
	if err != nil {
		t.Fatal(err)
	}
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	ctx, span := tp.Tracer("t").Start(context.Background(), "Settle")

	st := settlement.Settlement{
		Request: settlement.Request{ID: uuid.New(), TransferID: uuid.New(), Payer: uuid.New(), Payee: uuid.New(), AmountCents: 1},
		Status:  settlement.Settled,
	}
	obs.Settled(ctx, settlement.SettleEvent{Mode: settlement.ModeSettleNoResponse, Settlement: st, Held: true})
	obs.StatusQueried(ctx, st.ID, settlement.QueryFailed)
	span.End()

	m := collect(t, reader)
	if p := m["spi.settlements"]; len(p) != 1 || label(p[0], "spi.status") != "settled" || label(p[0], "spi.replay") != "false" {
		t.Fatalf("spi.settlements = %+v", p)
	}
	if p := m["spi.settle.held"]; len(p) != 1 || label(p[0], "spi.settle_mode") != "settle_no_response" {
		t.Fatalf("spi.settle.held = %+v", p)
	}
	if p := m["spi.status_queries"]; len(p) != 1 || label(p[0], "spi.query_result") != "failed" {
		t.Fatalf("spi.status_queries = %+v", p)
	}
	active := map[string]float64{}
	for _, p := range m["spi.behavior.settle_mode"] {
		active[label(p, "spi.settle_mode")] = p.value
	}
	if len(active) != 4 || active["settle_no_response"] != 1 || active["settle"] != 0 {
		t.Fatalf("modo em vigor = %v", active)
	}
	if p := m["spi.behavior.fail_status_queries"]; len(p) != 1 || p[0].value != 1 {
		t.Fatalf("fail_status_queries = %+v", p)
	}
	if p := m["spi.behavior.latency"]; len(p) != 1 || p[0].value != 0.5 {
		t.Fatalf("latência = %+v", p)
	}
	for name, ps := range m {
		for _, p := range ps {
			if p.attrs.HasValue("spi.settlement_id") || p.attrs.HasValue("spi.transfer_id") {
				t.Errorf("%s tem ID como rótulo", name)
			}
		}
	}
	got := map[string]string{}
	for _, kv := range spans.GetSpans()[0].Attributes {
		got[string(kv.Key)] = kv.Value.String()
	}
	if got["spi.settlement_id"] != st.ID.String() || got["spi.held"] != "true" || got["spi.query_result"] != "failed" {
		t.Fatalf("atributos do span = %v", got)
	}
}
