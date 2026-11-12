package grpcclient_test

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	pingv1 "conta.local/gen/go/conta/ping/v1"
	"conta.local/platform/grpcclient"
)

type echoTraceparent struct {
	pingv1.UnimplementedPingServiceServer
}

// Ping devolve o traceparent que chegou no metadata.
func (echoTraceparent) Ping(ctx context.Context, _ *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	var tp string
	if v := md.Get("traceparent"); len(v) > 0 {
		tp = v[0]
	}
	return &pingv1.PingResponse{Message: tp}, nil
}

func TestDialPropagatesTraceAndRecordsSpan(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pingv1.RegisterPingServiceServer(srv, echoTraceparent{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	conn, err := grpcclient.Dial("passthrough:///bufnet", grpcclient.Options{
		TracerProvider: tp, MeterProvider: noop.NewMeterProvider(), Propagator: propagation.TraceContext{},
	}, grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, parent := tp.Tracer("t").Start(t.Context(), "saga")
	resp, err := pingv1.NewPingServiceClient(conn).Ping(ctx, &pingv1.PingRequest{})
	parent.End()
	if err != nil {
		t.Fatal(err)
	}
	traceID := parent.SpanContext().TraceID().String()
	if got := resp.GetMessage(); len(got) < 36 || got[3:35] != traceID {
		t.Fatalf("traceparent recebido = %q, want o trace %s", got, traceID)
	}
	var client bool
	for _, s := range spans.GetSpans() {
		if s.Name == "conta.ping.v1.PingService/Ping" && s.Parent.SpanID() == parent.SpanContext().SpanID() {
			client = true
		}
	}
	if !client {
		t.Fatal("sem span de cliente filho do span da saga")
	}
}
