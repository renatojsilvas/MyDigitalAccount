// Package grpcapi é a borda gRPC do serviço: monta o servidor com seus interceptors,
// traduz erros para status gRPC e implementa os handlers. É a única camada que conhece
// códigos gRPC.
package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

const healthServicePrefix = "/grpc.health.v1."

// ServerOptions reúne o que o servidor precisa. Os providers entram por construtor,
// sem passar por globais do OpenTelemetry.
type ServerOptions struct {
	Logger         *slog.Logger
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
}

// NewServer cria o servidor gRPC com tracing, métricas RED, log e recuperação de panic.
//
// O otelgrpc, como stats handler, cria o span do servidor, extrai o traceparent da
// requisição e registra a duração das chamadas. Os interceptors rodam depois dele,
// então já enxergam o span no contexto (o log herda trace_id e span_id).
func NewServer(o ServerOptions, extra ...grpc.ServerOption) *grpc.Server {
	opts := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler(
			otelgrpc.WithTracerProvider(o.TracerProvider),
			otelgrpc.WithMeterProvider(o.MeterProvider),
			otelgrpc.WithPropagators(o.Propagator),
			// Sondas de saúde não entram nos traces nem nas métricas de negócio.
			otelgrpc.WithFilter(func(info *stats.RPCTagInfo) bool {
				return !strings.HasPrefix(info.FullMethodName, healthServicePrefix)
			}),
		)),
		// Logging por fora, recuperação por dentro: o panic vira erro Internal e o log
		// da chamada o registra com o código correto.
		grpc.ChainUnaryInterceptor(loggingInterceptor(o.Logger), recoveryInterceptor(o.Logger)),
	}
	return grpc.NewServer(append(opts, extra...)...)
}

func loggingInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := status.Code(err)

		level := slog.LevelInfo
		switch {
		case strings.HasPrefix(info.FullMethod, healthServicePrefix):
			level = slog.LevelDebug
		case isServerFault(code):
			level = slog.LevelError
		}
		logger.Log(ctx, level, "rpc",
			"method", info.FullMethod,
			"code", code.String(),
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
		)
		return resp, err
	}
}

func isServerFault(c codes.Code) bool {
	switch c {
	case codes.Unknown, codes.Internal, codes.DataLoss, codes.Unavailable, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

// recoveryInterceptor converte panic em erro Internal sem vazar detalhes ao cliente.
func recoveryInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.ErrorContext(ctx, "panic no handler", "method", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				err = status.Error(codes.Internal, "erro interno")
			}
		}()
		return handler(ctx, req)
	}
}

// Serve atende em lis até ctx ser cancelado e então para com graça: termina as chamadas
// em andamento e, se passar de shutdownTimeout, corta as que sobraram.
func Serve(ctx context.Context, srv *grpc.Server, lis net.Listener, shutdownTimeout time.Duration) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("servidor gRPC: %w", err)
	case <-ctx.Done():
	}

	stopped := make(chan struct{})
	go func() {
		srv.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(shutdownTimeout):
		srv.Stop()
	}
	// Se o sinal chegar antes de o Serve começar a aceitar, ele devolve ErrServerStopped:
	// é parada normal, não falha.
	if err := <-errCh; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("servidor gRPC: %w", err)
	}
	return nil
}
