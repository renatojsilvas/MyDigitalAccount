// Package grpcclient abre conexões gRPC entre os serviços Go com a instrumentação comum
// (ADR-0002, ADR-0010): um span de cliente por chamada, métricas RED do lado de quem chama e o
// traceparent propagado no metadata, para o trace atravessar os serviços.
package grpcclient

import (
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Options reúne os providers, entregues por construtor (sem os globais do OpenTelemetry).
type Options struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
}

// Dial cria a conexão (preguiçosa: conecta na primeira chamada). Sem TLS: a rede interna do
// compose é o limite de confiança do projeto, como entre BFF e ledger.
func Dial(target string, o Options, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(o.TracerProvider),
			otelgrpc.WithMeterProvider(o.MeterProvider),
			otelgrpc.WithPropagators(o.Propagator),
		)),
	}
	conn, err := grpc.NewClient(target, append(opts, extra...)...)
	if err != nil {
		return nil, fmt.Errorf("grpcclient: %s: %w", target, err)
	}
	return conn, nil
}
