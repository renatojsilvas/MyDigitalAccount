// Package logging monta o slog do serviço: JSON no stdout, com trace_id e span_id
// quando há span ativo no contexto, e redação de campos sensíveis na origem.
package logging

import (
	"context"
	"io"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// sensitiveKeys são nomes de atributo cujo valor nunca deve sair em log (CPF, token...).
var sensitiveKeys = map[string]bool{
	"cpf": true, "token": true, "password": true, "authorization": true, "pix_key": true,
}

const redacted = "[REDACTED]"

// New devolve um logger que escreve JSON em w. Se extra não for nil (por exemplo, o
// bridge do OpenTelemetry), cada registro vai também para ele.
func New(w io.Writer, level slog.Level, extra slog.Handler) *slog.Logger {
	var h slog.Handler = traceHandler{Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redact,
	})}
	if extra != nil {
		h = slog.NewMultiHandler(h, extra)
	}
	return slog.New(h)
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[a.Key] {
		return slog.String(a.Key, redacted)
	}
	return a
}

// traceHandler acrescenta trace_id e span_id para o log saltar para o trace no Grafana.
// Só funciona se o log for emitido com contexto (slog.InfoContext etc.).
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
