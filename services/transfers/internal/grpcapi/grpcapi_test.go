package grpcapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	transfersv1 "conta.local/gen/go/conta/transfers/v1"
	"conta.local/platform/grpcserver"
	"conta.local/platform/logging"
	"conta.local/transfers/internal/grpcapi"
	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/postgres"
	"conta.local/transfers/internal/postgres/pgtest"
)

type harness struct {
	client transfersv1.TransfersServiceClient
	store  *postgres.Store
	spans  *tracetest.InMemoryExporter
	tp     *sdktrace.TracerProvider
}

// newHarness monta a pilha inteira: gRPC (instrumentado) -> handler -> casos de uso -> Postgres.
func newHarness(t *testing.T) *harness {
	t.Helper()
	_, pool := pgtest.Migrated(t)
	cal, err := pix.NewCalendar()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{store: postgres.NewStore(pool), spans: tracetest.NewInMemoryExporter()}
	h.tp = sdktrace.NewTracerProvider(sdktrace.WithSyncer(h.spans))
	prop := propagation.TraceContext{}
	svc := pix.NewService(h.store, cal, []byte("segredo-de-teste-com-32-bytes-ok"), 200_000)
	logger := logging.New(&bytes.Buffer{}, slog.LevelDebug, nil)
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger: logger, TracerProvider: h.tp, MeterProvider: noop.NewMeterProvider(), Propagator: prop,
		Interceptors: []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
	})
	grpcapi.Register(srv, grpcapi.NewHandler(svc, func(ctx context.Context) map[string]string {
		c := propagation.MapCarrier{}
		prop.Inject(ctx, c)
		return c
	}))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler(otelgrpc.WithTracerProvider(h.tp), otelgrpc.WithPropagators(prop))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	h.client = transfersv1.NewTransfersServiceClient(conn)
	return h
}

func startRequest(account string) *transfersv1.StartPixRequest {
	return &transfersv1.StartPixRequest{
		IdempotencyKey: uuid.NewString(), AccountId: account,
		DestinationKey: &transfersv1.PixKey{Type: transfersv1.PixKeyType_PIX_KEY_TYPE_CPF, Value: "12345678909"},
		AmountCents:    25_000, Description: "aluguel",
	}
}

func reasonOf(err error) (codes.Code, string, string) {
	st := status.Convert(err)
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return st.Code(), info.GetReason(), info.GetMetadata()["field"]
		}
	}
	return st.Code(), "", ""
}

func TestStartAndGetPix(t *testing.T) {
	h := newHarness(t)
	account := uuid.NewString()
	req := startRequest(account)

	ctx, span := h.tp.Tracer("bff").Start(t.Context(), "POST /pix")
	started, err := h.client.StartPix(ctx, req)
	span.End()
	if err != nil {
		t.Fatal(err)
	}
	p := started.GetPix()
	if p.GetState() != transfersv1.PixState_PIX_STATE_ACCEPTED || p.GetStatus() != transfersv1.PixStatus_PIX_STATUS_PROCESSING ||
		p.GetDestinationKeyMasked() != "***.456.789-**" || p.GetAmountCents() != 25_000 || p.GetDestinationAccountId() != "" {
		t.Fatalf("StartPix = %v", p)
	}
	if strings.Contains(started.String(), "12345678909") {
		t.Fatal("a resposta contém a chave Pix completa")
	}

	again, err := h.client.StartPix(t.Context(), req)
	if err != nil || again.GetPix().GetPixId() != p.GetPixId() {
		t.Fatalf("repetição = %v, %v; want o mesmo Pix", again, err)
	}

	got, err := h.client.GetPix(t.Context(), &transfersv1.GetPixRequest{AccountId: account, PixId: p.GetPixId()})
	if err != nil {
		t.Fatal(err)
	}
	if hist := got.GetPix().GetHistory(); len(hist) != 1 || hist[0].GetToState() != transfersv1.PixState_PIX_STATE_ACCEPTED {
		t.Fatalf("histórico = %v", hist)
	}

	// O contexto de trace do aceite ficou gravado: a saga continuará o mesmo trace.
	accountID, _ := uuid.Parse(account)
	pixID, _ := uuid.Parse(p.GetPixId())
	stored, _, err := h.store.Get(t.Context(), accountID, pixID)
	if err != nil {
		t.Fatal(err)
	}
	if tp := stored.Trace["traceparent"]; !strings.Contains(tp, span.SpanContext().TraceID().String()) {
		t.Fatalf("traceparent gravado = %q, want o trace %s", tp, span.SpanContext().TraceID())
	}
}

func TestErrorsOverGRPC(t *testing.T) {
	h := newHarness(t)
	account := uuid.NewString()
	req := startRequest(account)
	if _, err := h.client.StartPix(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	reused := startRequest(account)
	reused.IdempotencyKey = req.GetIdempotencyKey()
	reused.AmountCents = 1

	badKey := startRequest(account)
	badKey.DestinationKey.Value = "11111111111"
	noType := startRequest(account)
	noType.DestinationKey.Type = transfersv1.PixKeyType_PIX_KEY_TYPE_UNSPECIFIED

	tests := []struct {
		name       string
		call       func() error
		wantCode   codes.Code
		wantReason string
		wantField  string
	}{
		{"chave reutilizada", func() error { _, err := h.client.StartPix(t.Context(), reused); return err },
			codes.AlreadyExists, "ERROR_REASON_IDEMPOTENCY_KEY_REUSED", ""},
		{"CPF inválido", func() error { _, err := h.client.StartPix(t.Context(), badKey); return err },
			codes.InvalidArgument, "ERROR_REASON_INVALID_REQUEST", "destination_key.value"},
		{"tipo de chave não informado", func() error { _, err := h.client.StartPix(t.Context(), noType); return err },
			codes.InvalidArgument, "ERROR_REASON_INVALID_REQUEST", "destination_key.type"},
		{"Pix inexistente", func() error {
			_, err := h.client.GetPix(t.Context(), &transfersv1.GetPixRequest{AccountId: account, PixId: uuid.NewString()})
			return err
		}, codes.NotFound, "ERROR_REASON_PIX_NOT_FOUND", ""},
		{"pix_id inválido", func() error {
			_, err := h.client.GetPix(t.Context(), &transfersv1.GetPixRequest{AccountId: account, PixId: "x"})
			return err
		}, codes.InvalidArgument, "ERROR_REASON_INVALID_REQUEST", "pix_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, reason, field := reasonOf(tt.call())
			if code != tt.wantCode || reason != tt.wantReason || field != tt.wantField {
				t.Fatalf("= %s %s %q, want %s %s %q", code, reason, field, tt.wantCode, tt.wantReason, tt.wantField)
			}
		})
	}
}

func TestGetLimits(t *testing.T) {
	h := newHarness(t)
	account := uuid.NewString()
	resp, err := h.client.GetLimits(t.Context(), &transfersv1.GetLimitsRequest{AccountId: account})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDailyLimitCents() != 200_000 || resp.GetAvailableTodayCents() != 200_000 || resp.GetUsedTodayCents() != 0 ||
		!resp.GetResetsAt().AsTime().After(time.Now()) {
		t.Fatalf("GetLimits = %v", resp)
	}
}
