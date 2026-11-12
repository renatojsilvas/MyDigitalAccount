// Package telemetry monta os três sinais do OpenTelemetry (traces, métricas e logs) e os
// exporta por OTLP para o Collector (ADR-0002). Os providers são devolvidos e passados por
// construtor; nada é registrado como global.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Config identifica o serviço e aponta para o Collector.
type Config struct {
	ServiceName string
	Version     string
	Environment string
	// InstanceID distingue réplicas do mesmo serviço (service.instance.id). Sem ele, duas
	// réplicas escreveriam séries idênticas no Prometheus. Em contêiner, o hostname já é único.
	InstanceID   string
	OTLPEndpoint string // vazio: nada é exportado (os providers continuam funcionando)
}

// Telemetry agrupa os providers e o propagador W3C (traceparent).
type Telemetry struct {
	Tracer     *sdktrace.TracerProvider
	Meter      *sdkmetric.MeterProvider
	Logger     *sdklog.LoggerProvider
	Propagator propagation.TextMapPropagator
}

type options struct {
	spanExporter sdktrace.SpanExporter
	metricReader sdkmetric.Reader
	logExporter  sdklog.Exporter
}

// Option altera a montagem; existe para os testes injetarem exportadores em memória.
type Option func(*options)

// WithSpanExporter troca o exportador de traces e passa a exportar de forma síncrona.
func WithSpanExporter(e sdktrace.SpanExporter) Option { return func(o *options) { o.spanExporter = e } }

// WithMetricReader troca o leitor de métricas (por exemplo, um ManualReader).
func WithMetricReader(r sdkmetric.Reader) Option { return func(o *options) { o.metricReader = r } }

// WithLogExporter troca o exportador de logs e passa a exportar de forma síncrona.
func WithLogExporter(e sdklog.Exporter) Option { return func(o *options) { o.logExporter = e } }

// New cria os providers. Sem Option e sem OTLPEndpoint, nada é exportado.
func New(ctx context.Context, cfg Config, opts ...Option) (*Telemetry, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", cfg.Version),
		attribute.String("deployment.environment.name", cfg.Environment),
	}
	if cfg.InstanceID != "" {
		attrs = append(attrs, attribute.String("service.instance.id", cfg.InstanceID))
	}
	res, err := resource.New(ctx, resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("criando resource: %w", err)
	}

	tracerOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	meterOpts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	logOpts := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}

	// Traces. Em produção o Batcher agrupa spans; nos testes o Syncer exporta na hora.
	switch {
	case o.spanExporter != nil:
		tracerOpts = append(tracerOpts, sdktrace.WithSyncer(o.spanExporter))
	case cfg.OTLPEndpoint != "":
		exp, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("exportador de traces: %w", err)
		}
		tracerOpts = append(tracerOpts, sdktrace.WithBatcher(exp))
	}

	// Métricas.
	switch {
	case o.metricReader != nil:
		meterOpts = append(meterOpts, sdkmetric.WithReader(o.metricReader))
	case cfg.OTLPEndpoint != "":
		exp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("exportador de métricas: %w", err)
		}
		meterOpts = append(meterOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(10*time.Second))))
	}

	// Logs.
	switch {
	case o.logExporter != nil:
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewSimpleProcessor(o.logExporter)))
	case cfg.OTLPEndpoint != "":
		exp, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, fmt.Errorf("exportador de logs: %w", err)
		}
		logOpts = append(logOpts, sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
	}

	t := &Telemetry{
		Tracer:     sdktrace.NewTracerProvider(tracerOpts...),
		Meter:      sdkmetric.NewMeterProvider(meterOpts...),
		Logger:     sdklog.NewLoggerProvider(logOpts...),
		Propagator: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}

	// Métricas de recurso (método USE): goroutines, heap, GC.
	if err := runtime.Start(runtime.WithMeterProvider(t.Meter)); err != nil {
		return nil, errors.Join(fmt.Errorf("métricas de runtime: %w", err), t.Shutdown(ctx))
	}
	return t, nil
}

// LogHandler devolve um slog.Handler que envia os logs ao provider de logs do OTel.
func (t *Telemetry) LogHandler(name string) slog.Handler {
	return otelslog.NewHandler(name, otelslog.WithLoggerProvider(t.Logger))
}

// Shutdown descarrega o que estiver em buffer e encerra os providers.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return errors.Join(t.Tracer.Shutdown(ctx), t.Meter.Shutdown(ctx), t.Logger.Shutdown(ctx))
}
