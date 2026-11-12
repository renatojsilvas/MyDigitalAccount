package grpcapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/platform/grpcserver"
	"conta.local/platform/logging"
	"conta.local/spi-fake/internal/grpcapi"
	"conta.local/spi-fake/internal/postgres"
	"conta.local/spi-fake/internal/postgres/pgtest"
	"conta.local/spi-fake/internal/settlement"
)

type clients struct {
	spi   spiv1.SpiServiceClient
	admin spiv1.SpiAdminServiceClient
}

// realSPI monta a pilha inteira: gRPC -> handler -> casos de uso -> Postgres.
func realSPI(t *testing.T, mode settlement.Mode) clients {
	t.Helper()
	_, pool := pgtest.Migrated(t)
	sw := settlement.NewSwitch(settlement.Behavior{Mode: mode})
	svc := settlement.NewService(postgres.NewStore(pool), sw, 5*time.Second)
	logger := logging.New(&bytes.Buffer{}, slog.LevelDebug, nil)
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger: logger, TracerProvider: tracenoop.NewTracerProvider(), MeterProvider: noop.NewMeterProvider(),
		Propagator: propagation.TraceContext{}, Interceptors: []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
	})
	grpcapi.Register(srv, grpcapi.NewHandler(svc, sw))
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return clients{spi: spiv1.NewSpiServiceClient(conn), admin: spiv1.NewSpiAdminServiceClient(conn)}
}

func settleRequest() *spiv1.SettleRequest {
	return &spiv1.SettleRequest{
		SettlementId: uuid.NewString(), TransferId: uuid.NewString(),
		PayerAccountId: uuid.NewString(), PayeeAccountId: uuid.NewString(), AmountCents: 5_000,
	}
}

func reasonOf(err error) (codes.Code, string) {
	st := status.Convert(err)
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return st.Code(), info.GetReason()
		}
	}
	return st.Code(), ""
}

func setMode(t *testing.T, c clients, b *spiv1.Behavior) {
	t.Helper()
	if _, err := c.admin.SetBehavior(t.Context(), &spiv1.SetBehaviorRequest{Behavior: b}); err != nil {
		t.Fatalf("SetBehavior: %v", err)
	}
}

// O fluxo da seção 4.5 pelo contrato: timeout com liquidação feita, consulta mostra liquidado,
// repetição com o mesmo ID devolve o mesmo resultado.
func TestTimeoutWithSettlementOverGRPC(t *testing.T) {
	c := realSPI(t, settlement.ModeSettleNoResponse)
	req := settleRequest()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.spi.Settle(ctx, req); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Settle = %v, want DEADLINE_EXCEEDED", err)
	}
	got, err := c.spi.GetSettlement(t.Context(), &spiv1.GetSettlementRequest{SettlementId: req.GetSettlementId()})
	if err != nil || got.GetSettlement().GetStatus() != spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED {
		t.Fatalf("GetSettlement = %v, %v; want SETTLED", got, err)
	}

	setMode(t, c, &spiv1.Behavior{SettleMode: spiv1.SettleMode_SETTLE_MODE_REJECT})
	again, err := c.spi.Settle(t.Context(), req)
	if err != nil || !proto.Equal(again.GetSettlement(), got.GetSettlement()) {
		t.Fatalf("repetição = %v, %v; want o liquidado registrado %v", again, err, got)
	}
}

func TestTimeoutWithoutSettlementOverGRPC(t *testing.T) {
	c := realSPI(t, settlement.ModeNoSettleNoResponse)
	req := settleRequest()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.spi.Settle(ctx, req); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Settle = %v, want DEADLINE_EXCEEDED", err)
	}
	_, err := c.spi.GetSettlement(t.Context(), &spiv1.GetSettlementRequest{SettlementId: req.GetSettlementId()})
	if code, reason := reasonOf(err); code != codes.NotFound || reason != "ERROR_REASON_SETTLEMENT_NOT_FOUND" {
		t.Fatalf("GetSettlement = %s %s, want NOT_FOUND SETTLEMENT_NOT_FOUND", code, reason)
	}
	// O pedido chega de novo (retry do transfers) e agora o SPI liquida, com o mesmo ID.
	setMode(t, c, &spiv1.Behavior{SettleMode: spiv1.SettleMode_SETTLE_MODE_SETTLE})
	resp, err := c.spi.Settle(t.Context(), req)
	if err != nil || resp.GetSettlement().GetStatus() != spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED {
		t.Fatalf("Settle = %v, %v", resp, err)
	}
}

func TestRejectAndErrorsOverGRPC(t *testing.T) {
	c := realSPI(t, settlement.ModeReject)
	req := settleRequest()
	resp, err := c.spi.Settle(t.Context(), req)
	if err != nil || resp.GetSettlement().GetStatus() != spiv1.SettlementStatus_SETTLEMENT_STATUS_REJECTED ||
		resp.GetSettlement().GetRejectReason() != spiv1.RejectReason_REJECT_REASON_SIMULATED {
		t.Fatalf("Settle = %v, %v; want REJECTED (SIMULATED)", resp, err)
	}

	reused := proto.Clone(req).(*spiv1.SettleRequest)
	reused.AmountCents = 1
	_, err = c.spi.Settle(t.Context(), reused)
	if code, reason := reasonOf(err); code != codes.AlreadyExists || reason != "ERROR_REASON_SETTLEMENT_ID_REUSED" {
		t.Fatalf("mesmo ID com outro valor = %s %s", code, reason)
	}

	bad := settleRequest()
	bad.PayeeAccountId = "x"
	_, err = c.spi.Settle(t.Context(), bad)
	if code, reason := reasonOf(err); code != codes.InvalidArgument || reason != "ERROR_REASON_INVALID_REQUEST" {
		t.Fatalf("pedido inválido = %s %s", code, reason)
	}

	setMode(t, c, &spiv1.Behavior{SettleMode: spiv1.SettleMode_SETTLE_MODE_SETTLE, FailStatusQueries: true})
	_, err = c.spi.GetSettlement(t.Context(), &spiv1.GetSettlementRequest{SettlementId: req.GetSettlementId()})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("consulta com falha simulada = %v, want UNAVAILABLE", err)
	}
}

func TestAdminBehavior(t *testing.T) {
	c := realSPI(t, settlement.ModeSettle)
	want := &spiv1.Behavior{
		SettleMode: spiv1.SettleMode_SETTLE_MODE_REJECT, FailStatusQueries: true, Latency: durationpb.New(250 * time.Millisecond),
	}
	setMode(t, c, want)
	got, err := c.admin.GetBehavior(t.Context(), &spiv1.GetBehaviorRequest{})
	if err != nil || !proto.Equal(got.GetBehavior(), want) {
		t.Fatalf("GetBehavior = %v, %v; want %v", got, err, want)
	}
	for _, bad := range []*spiv1.Behavior{
		{}, {SettleMode: 99}, {SettleMode: spiv1.SettleMode_SETTLE_MODE_SETTLE, Latency: durationpb.New(-time.Second)},
	} {
		_, err := c.admin.SetBehavior(t.Context(), &spiv1.SetBehaviorRequest{Behavior: bad})
		if code, _ := reasonOf(err); code != codes.InvalidArgument {
			t.Errorf("SetBehavior(%v) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	if got, _ := c.admin.GetBehavior(t.Context(), &spiv1.GetBehaviorRequest{}); !proto.Equal(got.GetBehavior(), want) {
		t.Fatalf("um SetBehavior inválido mudou o comportamento: %v", got)
	}
}
