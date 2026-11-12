package grpcapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
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
	"google.golang.org/protobuf/types/known/durationpb"

	"conta.local/antifraude/internal/faults"
	"conta.local/antifraude/internal/grpcapi"
	"conta.local/antifraude/internal/risk"
	antifraudev1 "conta.local/gen/go/conta/antifraude/v1"
	"conta.local/platform/grpcserver"
	"conta.local/platform/logging"
)

type fakeEvaluator struct {
	a   risk.Assessment
	err error
}

func (f fakeEvaluator) Evaluate(context.Context, risk.Request) (risk.Assessment, error) {
	return f.a, f.err
}

type clients struct {
	svc   antifraudev1.AntifraudeServiceClient
	admin antifraudev1.AntifraudeAdminServiceClient
}

func newClients(t *testing.T, ev grpcapi.Evaluator, inj *faults.Injector, logs *bytes.Buffer) clients {
	t.Helper()
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	logger := logging.New(logs, slog.LevelDebug, nil)
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger: logger, TracerProvider: tracenoop.NewTracerProvider(), MeterProvider: noop.NewMeterProvider(),
		Propagator: propagation.TraceContext{}, Interceptors: []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
	})
	grpcapi.Register(srv, grpcapi.NewHandler(ev, inj))
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
	return clients{svc: antifraudev1.NewAntifraudeServiceClient(conn), admin: antifraudev1.NewAntifraudeAdminServiceClient(conn)}
}

func validRequest() *antifraudev1.EvaluateRequest {
	return &antifraudev1.EvaluateRequest{TransferId: uuid.NewString(), AccountId: uuid.NewString(), AmountCents: 1_000}
}

func TestEvaluate(t *testing.T) {
	ev := fakeEvaluator{a: risk.Assessment{
		Decision: risk.Deny, Score: 100, Degraded: true,
		Signals: []risk.Signal{risk.SignalHighAmount, risk.SignalVelocityUnavailable},
	}}
	c := newClients(t, ev, faults.New(faults.Config{}), nil)
	resp, err := c.svc.Evaluate(t.Context(), validRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDecision() != antifraudev1.Decision_DECISION_DENY || resp.GetScore() != 100 || !resp.GetDegraded() ||
		len(resp.GetSignals()) != 2 || resp.GetSignals()[1] != antifraudev1.RiskSignal_RISK_SIGNAL_VELOCITY_UNAVAILABLE {
		t.Fatalf("resposta = %v", resp)
	}
}

func fieldOf(err error) (codes.Code, string) {
	st := status.Convert(err)
	for _, d := range st.Details() {
		if br, ok := d.(*errdetails.BadRequest); ok && len(br.GetFieldViolations()) > 0 {
			return st.Code(), br.GetFieldViolations()[0].GetField()
		}
	}
	return st.Code(), ""
}

func TestEvaluateValidation(t *testing.T) {
	c := newClients(t, fakeEvaluator{err: errors.New("não deveria ser chamado")}, faults.New(faults.Config{}), nil)
	tests := []struct {
		name  string
		edit  func(*antifraudev1.EvaluateRequest)
		field string
	}{
		{"transfer_id", func(r *antifraudev1.EvaluateRequest) { r.TransferId = "x" }, "transfer_id"},
		{"account_id", func(r *antifraudev1.EvaluateRequest) { r.AccountId = "" }, "account_id"},
		{"valor", func(r *antifraudev1.EvaluateRequest) { r.AmountCents = -1 }, "amount_cents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validRequest()
			tt.edit(r)
			_, err := c.svc.Evaluate(t.Context(), r)
			if code, field := fieldOf(err); code != codes.InvalidArgument || field != tt.field {
				t.Fatalf("= %s %q, want INVALID_ARGUMENT em %s", code, field, tt.field)
			}
		})
	}
}

func TestFaultsOverGRPC(t *testing.T) {
	inj := faults.New(faults.Config{})
	c := newClients(t, fakeEvaluator{a: risk.Assessment{Decision: risk.Approve}}, inj, nil)

	// Taxa de erro 1: toda avaliação falha com UNAVAILABLE.
	set, err := c.admin.SetFaults(t.Context(), &antifraudev1.SetFaultsRequest{Faults: &antifraudev1.Faults{ErrorRate: 1}})
	if err != nil || set.GetFaults().GetErrorRate() != 1 {
		t.Fatalf("SetFaults = %v, %v", set, err)
	}
	if _, err := c.svc.Evaluate(t.Context(), validRequest()); status.Code(err) != codes.Unavailable {
		t.Fatalf("err = %v, want UNAVAILABLE", err)
	}

	// Latência acima do prazo do cliente: DEADLINE_EXCEEDED, sem esperar a latência inteira.
	if _, err := c.admin.SetFaults(t.Context(), &antifraudev1.SetFaultsRequest{
		Faults: &antifraudev1.Faults{Latency: durationpb.New(5 * time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.svc.Evaluate(ctx, validRequest()); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("err = %v, want DEADLINE_EXCEEDED", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("esperou %v: a latência injetada deve respeitar o prazo do cliente", time.Since(start))
	}

	got, err := c.admin.GetFaults(t.Context(), &antifraudev1.GetFaultsRequest{})
	if err != nil || got.GetFaults().GetLatency().AsDuration() != 5*time.Second || got.GetFaults().GetErrorRate() != 0 {
		t.Fatalf("GetFaults = %v, %v", got, err)
	}

	// Valores inválidos são recusados e não mudam nada.
	for _, bad := range []*antifraudev1.Faults{{ErrorRate: 2}, {Latency: durationpb.New(-time.Second)}} {
		_, err := c.admin.SetFaults(t.Context(), &antifraudev1.SetFaultsRequest{Faults: bad})
		if code, _ := fieldOf(err); code != codes.InvalidArgument {
			t.Fatalf("SetFaults(%v) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	if inj.Current().Latency != 5*time.Second {
		t.Fatalf("falha inválida alterou a configuração: %+v", inj.Current())
	}
}

func TestInternalErrorIsLoggedNotLeaked(t *testing.T) {
	var logs bytes.Buffer
	c := newClients(t, fakeEvaluator{err: errors.New("redis: segredo no erro")}, faults.New(faults.Config{}), &logs)
	_, err := c.svc.Evaluate(t.Context(), validRequest())
	if st := status.Convert(err); st.Code() != codes.Internal || st.Message() != "erro interno" {
		t.Fatalf("cliente recebeu %s %q", st.Code(), st.Message())
	}
	if !strings.Contains(logs.String(), "redis: segredo no erro") {
		t.Fatalf("a causa não foi para o log: %s", logs.String())
	}
}
