package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"conta.local/ledger/internal/platform/logging"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log não é JSON válido: %v\n%s", err, buf)
	}
	return m
}

func TestTraceFields(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	spanID, _ := trace.SpanIDFromHex("0102030405060708")
	withSpan := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled}))

	tests := []struct {
		name      string
		ctx       context.Context
		wantTrace bool
	}{
		{"com span ativo", withSpan, true},
		{"sem span", context.Background(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logging.New(&buf, slog.LevelInfo, nil).InfoContext(tt.ctx, "oi")
			got := decode(t, &buf)
			_, hasTrace := got["trace_id"]
			_, hasSpan := got["span_id"]
			if hasTrace != tt.wantTrace || hasSpan != tt.wantTrace {
				t.Fatalf("trace_id=%v span_id=%v, esperado presença=%v: %v", hasTrace, hasSpan, tt.wantTrace, got)
			}
			if tt.wantTrace && (got["trace_id"] != traceID.String() || got["span_id"] != spanID.String()) {
				t.Errorf("ids errados: %v", got)
			}
		})
	}
}

func TestRedaction(t *testing.T) {
	var buf bytes.Buffer
	logging.New(&buf, slog.LevelInfo, nil).InfoContext(t.Context(), "x", "cpf", "123.456.789-09", "pix_key", "a@b.com", "account", "ok")
	got := decode(t, &buf)
	if got["cpf"] != "[REDACTED]" || got["pix_key"] != "[REDACTED]" {
		t.Errorf("campos sensíveis vazaram: %v", got)
	}
	if got["account"] != "ok" {
		t.Errorf("campo comum foi alterado: %v", got)
	}
}

func TestLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	logging.New(&buf, slog.LevelWarn, nil).InfoContext(t.Context(), "ignorado")
	if buf.Len() != 0 {
		t.Errorf("log abaixo do nível foi escrito: %s", buf.String())
	}
}
