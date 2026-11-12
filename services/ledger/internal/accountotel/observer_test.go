package accountotel_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/accountotel"
)

type point struct {
	value int64
	attrs attribute.Set
}

// collect devolve, por nome de métrica, os pontos de cada série.
func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string][]point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string][]point{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				out[m.Name] = append(out[m.Name], point{value: dp.Value, attrs: dp.Attributes})
			}
		}
	}
	return out
}

func TestWriteFinished(t *testing.T) {
	id := account.ID{15: 7}
	amount := account.MoneyFromCents(2_500)
	tests := []struct {
		name       string
		ev         account.WriteEvent
		wantMetric string
		wantValue  int64
		wantAttrs  map[string]string
		wantSpan   map[string]string
	}{
		{
			name:       "débito executado",
			ev:         account.WriteEvent{Operation: "debit", Outcome: account.OutcomeExecuted, Reason: "external", Amount: amount, AccountID: id},
			wantMetric: "ledger.posted_amount", wantValue: 2_500,
			wantAttrs: map[string]string{"ledger.operation": "debit", "ledger.reason": "external"},
			wantSpan:  map[string]string{"ledger.outcome": "executed", "ledger.account_id": id.String()},
		},
		{
			name:       "lançamento contado",
			ev:         account.WriteEvent{Operation: "credit", Outcome: account.OutcomeExecuted, Reason: "opening", Amount: amount, AccountID: id},
			wantMetric: "ledger.entries", wantValue: 1,
			wantAttrs: map[string]string{"ledger.operation": "credit", "ledger.reason": "opening"},
		},
		{
			name:       "débito recusado por saldo",
			ev:         account.WriteEvent{Operation: "debit", Outcome: account.OutcomeRejected, Rejection: account.ErrInsufficientFunds, Reason: "external", AccountID: id},
			wantMetric: "ledger.rejections", wantValue: 1,
			wantAttrs: map[string]string{"ledger.operation": "debit", "ledger.rejection": "insufficient_funds"},
			wantSpan:  map[string]string{"ledger.rejection": "insufficient_funds"},
		},
		{
			name:       "repetição",
			ev:         account.WriteEvent{Operation: "debit", Outcome: account.OutcomeReplayed, AccountID: id},
			wantMetric: "ledger.idempotency.replays", wantValue: 1,
			wantAttrs: map[string]string{"ledger.operation": "debit"},
		},
		{
			name:       "conflito de chave",
			ev:         account.WriteEvent{Operation: "credit", Outcome: account.OutcomeKeyConflict, AccountID: id},
			wantMetric: "ledger.idempotency.conflicts", wantValue: 1,
			wantAttrs: map[string]string{"ledger.operation": "credit"},
		},
		{
			name:       "conta aberta",
			ev:         account.WriteEvent{Operation: "open_account", Outcome: account.OutcomeExecuted, AccountID: id},
			wantMetric: "ledger.accounts.opened", wantValue: 1,
			wantAttrs: map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			obs, err := accountotel.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
			if err != nil {
				t.Fatal(err)
			}
			spans := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
			ctx, span := tp.Tracer("t").Start(context.Background(), "rpc")

			obs.WriteFinished(ctx, tt.ev)
			span.End()

			metrics := collect(t, reader)
			points := metrics[tt.wantMetric]
			if len(points) != 1 || points[0].value != tt.wantValue {
				t.Fatalf("%s = %+v, want um ponto com %d", tt.wantMetric, points, tt.wantValue)
			}
			for k, v := range tt.wantAttrs {
				if got, _ := points[0].attrs.Value(attribute.Key(k)); got.AsString() != v {
					t.Errorf("rótulo %s = %q, want %q", k, got.AsString(), v)
				}
			}
			if points[0].attrs.Len() != len(tt.wantAttrs) {
				t.Errorf("rótulos = %v, want só %v", points[0].attrs.ToSlice(), tt.wantAttrs)
			}
			// Cardinalidade: o ID da conta nunca vira rótulo de métrica.
			for name, ps := range metrics {
				for _, p := range ps {
					if p.attrs.HasValue("ledger.account_id") {
						t.Errorf("%s tem ledger.account_id como rótulo", name)
					}
				}
			}

			got := map[string]string{}
			for _, kv := range spans.GetSpans()[0].Attributes {
				got[string(kv.Key)] = kv.Value.String()
			}
			for k, v := range tt.wantSpan {
				if got[k] != v {
					t.Errorf("atributo de span %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
