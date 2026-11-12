// Package grpcclient abre conexões gRPC entre os serviços Go com a instrumentação comum
// (ADR-0002, ADR-0010): um span de cliente por chamada, métricas RED do lado de quem chama e o
// traceparent propagado no metadata, para o trace atravessar os serviços.
package grpcclient

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
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

// WarmUp abre a conexão e espera ela ficar pronta, até timeout, para que a primeira chamada de
// verdade não pague a partida a frio (resolução de nome, TCP e o handshake do HTTP/2). Nunca
// falha o boot: devolve false se a conexão não ficou pronta no prazo, e quem chama só registra
// o aviso (a dependência pode subir depois; o gRPC reconecta sozinho).
func WarmUp(ctx context.Context, conn *grpc.ClientConn, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn.Connect()
	for {
		s := conn.GetState()
		if s == connectivity.Ready {
			return true
		}
		if !conn.WaitForStateChange(ctx, s) {
			return false
		}
	}
}
