package pixotel_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/pixotel"
)

func TestObserver(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	obs, err := pixotel.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	if err != nil {
		t.Fatal(err)
	}
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	ctx, span := tp.Tracer("t").Start(context.Background(), "StartPix")

	key, _ := pix.NewKey(pix.KeyCPF, "12345678909")
	tr := pix.Transfer{ID: uuid.New(), SourceAccountID: uuid.New(), DestinationKey: key, AmountCents: 2_500, State: pix.StateAccepted}
	obs.Started(ctx, tr, true)
	obs.Started(ctx, tr, false)
	obs.KeyConflict(ctx)
	span.End()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				name := m.Name
				if v, ok := p.Attributes.Value("transfers.replay"); ok {
					name += "{replay=" + v.String() + "}"
				}
				if p.Attributes.HasValue("transfers.pix_id") || p.Attributes.HasValue("transfers.account_id") {
					t.Errorf("%s tem ID como rótulo", m.Name)
				}
				got[name] = p.Value
			}
		}
	}
	want := map[string]int64{
		"transfers.pix.started{replay=false}": 1, "transfers.pix.started{replay=true}": 1,
		"transfers.pix.started_amount": 2_500, "transfers.idempotency.conflicts": 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (todas: %v)", k, got[k], v, got)
		}
	}
	for _, kv := range spans.GetSpans()[0].Attributes {
		if strings.Contains(kv.Value.String(), "12345678909") {
			t.Fatalf("o span tem a chave Pix completa em %s", kv.Key)
		}
		if kv.Key == attribute.Key("transfers.destination_key") && kv.Value.String() != "***.456.789-**" {
			t.Fatalf("chave no span = %q", kv.Value.String())
		}
	}
}
