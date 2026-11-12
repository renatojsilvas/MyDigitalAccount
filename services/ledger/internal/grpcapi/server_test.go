package grpcapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"log/slog"

	pingv1 "conta.local/gen/go/conta/ping/v1"
	"conta.local/ledger/internal/grpcapi"
	"conta.local/ledger/internal/platform/logging"
	"conta.local/ledger/internal/platform/telemetry"
)

type harness struct {
	client  pingv1.PingServiceClient
	health  healthpb.HealthClient
	spans   *tracetest.InMemoryExporter
	reader  *metric.ManualReader
	logs    *bytes.Buffer
	tel     *telemetry.Telemetry
	cleanup func()
}

type panicHandler struct {
	pingv1.UnimplementedPingServiceServer
}

func (panicHandler) Ping(context.Context, *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	panic("boom")
}

func newHarness(t *testing.T, ping pingv1.PingServiceServer) *harness {
	t.Helper()
	ctx := context.Background()
	h := &harness{spans: tracetest.NewInMemoryExporter(), reader: metric.NewManualReader(), logs: &bytes.Buffer{}}

	tel, err := telemetry.New(ctx, telemetry.Config{ServiceName: "ledger", Environment: "test"},
		telemetry.WithSpanExporter(h.spans), telemetry.WithMetricReader(h.reader))
	if err != nil {
		t.Fatal(err)
	}
	h.tel = tel
	logger := logging.New(h.logs, slog.LevelDebug, nil)

	srv := grpcapi.NewServer(grpcapi.ServerOptions{
		Logger: logger, TracerProvider: tel.Tracer, MeterProvider: tel.Meter, Propagator: tel.Propagator,
	})
	grpcapi.RegisterPing(srv, ping)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)

	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()

	// O cliente também é instrumentado: é ele que injeta o traceparent na chamada.
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(
			otelgrpc.WithTracerProvider(tel.Tracer), otelgrpc.WithMeterProvider(tel.Meter), otelgrpc.WithPropagators(tel.Propagator))),
	)
	if err != nil {
		t.Fatal(err)
	}
	h.client = pingv1.NewPingServiceClient(conn)
	h.health = healthpb.NewHealthClient(conn)
	h.cleanup = func() {
		_ = conn.Close()
		srv.Stop()
		_ = tel.Shutdown(ctx)
	}
	t.Cleanup(h.cleanup)
	return h
}

func TestPing(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantCode codes.Code
	}{
		{"eco da mensagem", "olá", codes.OK},
		{"mensagem vazia", "", codes.OK},
		{"no limite de 256 caracteres", strings.Repeat("a", 256), codes.OK},
		{"acima do limite", strings.Repeat("a", 257), codes.InvalidArgument},
	}
	h := newHarness(t, grpcapi.NewPingHandler("ledger"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := h.client.Ping(t.Context(), &pingv1.PingRequest{Message: tt.message})
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("código %v, esperado %v (err=%v)", got, tt.wantCode, err)
			}
			if tt.wantCode == codes.OK && (resp.GetMessage() != tt.message || resp.GetService() != "ledger") {
				t.Errorf("resposta inesperada: %v", resp)
			}
		})
	}
}

func TestPingEmitsTraceLogAndMetric(t *testing.T) {
	h := newHarness(t, grpcapi.NewPingHandler("ledger"))
	if _, err := h.client.Ping(t.Context(), &pingv1.PingRequest{Message: "x"}); err != nil {
		t.Fatal(err)
	}

	// Trace: há um span de servidor e um de cliente, no mesmo trace (traceparent propagado).
	// O span e a métrica do servidor são fechados no fim do RPC (stats handler), que pode
	// acontecer logo depois de o cliente receber a resposta: espera, com prazo, em vez de ler
	// na hora.
	var server, client tracetest.SpanStub
	eventually(t, "spans de servidor e de cliente", func() bool {
		for _, s := range h.spans.GetSpans() {
			switch s.SpanKind.String() {
			case "server":
				server = s
			case "client":
				client = s
			}
		}
		return server.Name != "" && client.Name != ""
	})
	if server.Name != "conta.ping.v1.PingService/Ping" {
		t.Fatalf("span de servidor ausente ou com nome inesperado: %q", server.Name)
	}
	if server.SpanContext.TraceID() != client.SpanContext.TraceID() || server.Parent.SpanID() != client.SpanContext.SpanID() {
		t.Errorf("traceparent não foi propagado: servidor trace=%s pai=%s, cliente trace=%s span=%s",
			server.SpanContext.TraceID(), server.Parent.SpanID(), client.SpanContext.TraceID(), client.SpanContext.SpanID())
	}
	hasRPCAttr := false
	for _, a := range server.Attributes {
		if strings.HasPrefix(string(a.Key), "rpc.") {
			hasRPCAttr = true
		}
	}
	if !hasRPCAttr {
		t.Errorf("span sem atributos rpc.*: %v", server.Attributes)
	}

	// Log: o registro da chamada carrega o trace_id do mesmo span.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(h.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("linha de log inválida: %v: %s", err, line)
		}
		if rec["msg"] == "rpc" && rec["method"] == "/conta.ping.v1.PingService/Ping" {
			found = true
			if rec["trace_id"] != server.SpanContext.TraceID().String() || rec["span_id"] != server.SpanContext.SpanID().String() {
				t.Errorf("log sem correlação com o span: %v", rec)
			}
			if rec["code"] != "OK" {
				t.Errorf("code = %v", rec["code"])
			}
		}
	}
	if !found {
		t.Errorf("log da chamada não encontrado:\n%s", h.logs)
	}

	// Métricas: a duração do RPC de servidor foi registrada (método RED).
	names := map[string]bool{}
	var hasRPC, hasRuntime bool
	eventually(t, "métrica rpc.server.*", func() bool {
		var rm metricdata.ResourceMetrics
		if err := h.reader.Collect(t.Context(), &rm); err != nil {
			t.Fatal(err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				names[m.Name] = true
			}
		}
		for n := range names {
			hasRPC = hasRPC || strings.HasPrefix(n, "rpc.server.")
			hasRuntime = hasRuntime || strings.HasPrefix(n, "go.")
		}
		return hasRPC
	})
	if !hasRPC {
		t.Errorf("métrica rpc.server.* ausente: %v", names)
	}
	if !hasRuntime {
		t.Errorf("métricas de runtime (go.*) ausentes: %v", names)
	}
}

func TestHealthCheckIsNotTraced(t *testing.T) {
	h := newHarness(t, grpcapi.NewPingHandler("ledger"))
	resp, err := h.health.Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v", resp.GetStatus())
	}
	for _, s := range h.spans.GetSpans() {
		if strings.Contains(s.Name, "grpc.health") && s.SpanKind.String() == "server" {
			t.Errorf("a sonda de saúde gerou span de servidor: %s", s.Name)
		}
	}
}

func TestPanicBecomesInternal(t *testing.T) {
	h := newHarness(t, panicHandler{})
	_, err := h.client.Ping(t.Context(), &pingv1.PingRequest{Message: "x"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("código %v, esperado Internal (err=%v)", status.Code(err), err)
	}
	if strings.Contains(err.Error(), "boom") {
		t.Errorf("detalhe do panic vazou ao cliente: %v", err)
	}
	if !strings.Contains(h.logs.String(), "panic no handler") || !strings.Contains(h.logs.String(), `"code":"Internal"`) {
		t.Errorf("panic ou código não registrados no log:\n%s", h.logs)
	}
}

func TestServeGracefulShutdown(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- grpcapi.Serve(ctx, srv, lis, time.Second) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve devolveu erro no encerramento normal: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve não encerrou após cancelar o contexto")
	}
}

// eventually espera cond ficar verdadeira, conferindo a cada 10ms, por até 2s. Para efeitos
// assíncronos (span e métrica fechados depois da resposta), sem sleep fixo no teste.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Errorf("%s: não aconteceu em 2s", what)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
