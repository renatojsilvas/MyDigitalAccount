// Package grpcapi é a borda gRPC do antifraude: converte os pedidos, aplica as falhas
// injetadas e traduz erros para status gRPC. É a única camada do serviço que conhece gRPC.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"conta.local/antifraude/internal/faults"
	"conta.local/antifraude/internal/risk"
	antifraudev1 "conta.local/gen/go/conta/antifraude/v1"
)

// Evaluator é o caso de uso que a borda chama, definido aqui, por quem consome.
type Evaluator interface {
	Evaluate(ctx context.Context, req risk.Request) (risk.Assessment, error)
}

// Handler implementa AntifraudeService e AntifraudeAdminService.
type Handler struct {
	antifraudev1.UnimplementedAntifraudeServiceServer
	antifraudev1.UnimplementedAntifraudeAdminServiceServer
	svc    Evaluator
	faults *faults.Injector
}

// NewHandler cria o handler sobre o caso de uso e o injetor de falhas.
func NewHandler(svc Evaluator, inj *faults.Injector) *Handler {
	return &Handler{svc: svc, faults: inj}
}

// Register registra os dois serviços no servidor.
func Register(srv grpc.ServiceRegistrar, h *Handler) {
	antifraudev1.RegisterAntifraudeServiceServer(srv, h)
	antifraudev1.RegisterAntifraudeAdminServiceServer(srv, h)
}

// Evaluate avalia um Pix. As falhas injetadas vêm antes de tudo: simulam o serviço inteiro
// lento ou fora do ar, não só o Redis.
func (h *Handler) Evaluate(ctx context.Context, req *antifraudev1.EvaluateRequest) (*antifraudev1.EvaluateResponse, error) {
	if err := h.faults.Apply(ctx); err != nil {
		return nil, err
	}
	r, err := risk.NewRequest(req.GetTransferId(), req.GetAccountId(), req.GetAmountCents())
	if err != nil {
		return nil, err
	}
	a, err := h.svc.Evaluate(ctx, r)
	if err != nil {
		return nil, err
	}
	resp := &antifraudev1.EvaluateResponse{
		Decision: decisionToProto(a.Decision),
		Score:    int32(a.Score), //nolint:gosec // score entre 0 e 100
		Degraded: a.Degraded,
	}
	for _, s := range a.Signals {
		resp.Signals = append(resp.Signals, signalToProto(s))
	}
	return resp, nil
}

// SetFaults troca as falhas injetadas.
func (h *Handler) SetFaults(_ context.Context, req *antifraudev1.SetFaultsRequest) (*antifraudev1.SetFaultsResponse, error) {
	f := req.GetFaults()
	if err := f.GetLatency().CheckValid(); f.GetLatency() != nil && err != nil {
		return nil, invalidField("faults.latency", "is not a valid duration")
	}
	cfg := faults.Config{Latency: f.GetLatency().AsDuration(), ErrorRate: f.GetErrorRate()}
	if err := h.faults.Set(cfg); err != nil {
		return nil, invalidField("faults", err.Error())
	}
	return &antifraudev1.SetFaultsResponse{Faults: faultsToProto(h.faults.Current())}, nil
}

// GetFaults devolve as falhas em vigor.
func (h *Handler) GetFaults(context.Context, *antifraudev1.GetFaultsRequest) (*antifraudev1.GetFaultsResponse, error) {
	return &antifraudev1.GetFaultsResponse{Faults: faultsToProto(h.faults.Current())}, nil
}

func faultsToProto(c faults.Config) *antifraudev1.Faults {
	return &antifraudev1.Faults{Latency: durationpb.New(c.Latency), ErrorRate: c.ErrorRate}
}

func decisionToProto(d risk.Decision) antifraudev1.Decision {
	switch d {
	case risk.Approve:
		return antifraudev1.Decision_DECISION_APPROVE
	case risk.Deny:
		return antifraudev1.Decision_DECISION_DENY
	default:
		return antifraudev1.Decision_DECISION_UNSPECIFIED
	}
}

func signalToProto(s risk.Signal) antifraudev1.RiskSignal {
	switch s {
	case risk.SignalHighAmount:
		return antifraudev1.RiskSignal_RISK_SIGNAL_HIGH_AMOUNT
	case risk.SignalHighVelocityCount:
		return antifraudev1.RiskSignal_RISK_SIGNAL_HIGH_VELOCITY_COUNT
	case risk.SignalHighVelocityAmount:
		return antifraudev1.RiskSignal_RISK_SIGNAL_HIGH_VELOCITY_AMOUNT
	case risk.SignalVelocityUnavailable:
		return antifraudev1.RiskSignal_RISK_SIGNAL_VELOCITY_UNAVAILABLE
	default:
		return antifraudev1.RiskSignal_RISK_SIGNAL_UNSPECIFIED
	}
}

// invalidField monta INVALID_ARGUMENT com o campo culpado num google.rpc.BadRequest (tipo
// padrão do googleapis), para o cliente não precisar interpretar a mensagem.
func invalidField(field, problem string) error {
	return invalidStatus(field, problem).Err()
}

func invalidStatus(field, problem string) *status.Status {
	st := status.New(codes.InvalidArgument, field+": "+problem)
	withDetails, err := st.WithDetails(&errdetails.BadRequest{
		FieldViolations: []*errdetails.BadRequest_FieldViolation{{Field: field, Description: problem}},
	})
	if err != nil {
		// Só falha se o detalhe não puder ser serializado, o que não acontece com BadRequest.
		return st
	}
	return withDetails
}

// statusFor traduz um erro para o status devolvido ao cliente. O segundo retorno é falso quando
// o erro não é esperado: o cliente recebe só "erro interno" e a causa vai para o log.
func statusFor(err error) (*status.Status, bool) {
	var grpcErr interface{ GRPCStatus() *status.Status }
	var verr *risk.ValidationError
	switch {
	case errors.As(err, &grpcErr):
		return grpcErr.GRPCStatus(), true
	case errors.As(err, &verr):
		return invalidStatus(verr.Field, verr.Problem), true
	case errors.Is(err, faults.ErrInjected):
		return status.New(codes.Unavailable, "falha injetada"), true
	case errors.Is(err, context.DeadlineExceeded):
		return status.New(codes.DeadlineExceeded, "prazo da chamada esgotado"), true
	case errors.Is(err, context.Canceled):
		return status.New(codes.Canceled, "chamada cancelada"), true
	default:
		return status.New(codes.Internal, "erro interno"), false
	}
}

// ErrorInterceptor é o tratamento central de erros: os handlers devolvem erros de domínio e ele
// os traduz. Erros inesperados são registrados aqui, com a causa, no log ligado ao trace.
func ErrorInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		resp, err := handler(ctx, req)
		if err == nil {
			return resp, nil
		}
		st, expected := statusFor(err)
		if !expected {
			logger.ErrorContext(ctx, "erro interno", "method", info.FullMethod, "error", err.Error())
		}
		return nil, st.Err()
	}
}
