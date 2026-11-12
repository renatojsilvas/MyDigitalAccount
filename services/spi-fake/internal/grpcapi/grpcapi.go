// Package grpcapi é a borda gRPC do spi-fake: SpiService (liquidar e consultar) e
// SpiAdminService (comportamento do simulador). Traduz erros de domínio para status gRPC com
// google.rpc.ErrorInfo (reason = nome de spiv1.ErrorReason). Única camada que conhece gRPC.
package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/spi-fake/internal/settlement"
)

const errorDomain = "spi.conta.local"

// Settlements são os casos de uso que a borda chama, definidos aqui, por quem consome.
type Settlements interface {
	Settle(ctx context.Context, req settlement.Request) (settlement.Settlement, error)
	Get(ctx context.Context, id uuid.UUID) (settlement.Settlement, error)
}

// Handler implementa SpiService e SpiAdminService.
type Handler struct {
	spiv1.UnimplementedSpiServiceServer
	spiv1.UnimplementedSpiAdminServiceServer
	svc      Settlements
	behavior *settlement.Switch
}

// NewHandler cria o handler.
func NewHandler(svc Settlements, behavior *settlement.Switch) *Handler {
	return &Handler{svc: svc, behavior: behavior}
}

// Register registra os dois serviços no servidor.
func Register(srv grpc.ServiceRegistrar, h *Handler) {
	spiv1.RegisterSpiServiceServer(srv, h)
	spiv1.RegisterSpiAdminServiceServer(srv, h)
}

// Settle pede a liquidação.
func (h *Handler) Settle(ctx context.Context, req *spiv1.SettleRequest) (*spiv1.SettleResponse, error) {
	r, err := settlement.NewRequest(req.GetSettlementId(), req.GetTransferId(),
		req.GetPayerAccountId(), req.GetPayeeAccountId(), req.GetAmountCents())
	if err != nil {
		return nil, err
	}
	st, err := h.svc.Settle(ctx, r)
	if err != nil {
		return nil, err
	}
	return &spiv1.SettleResponse{Settlement: settlementToProto(st)}, nil
}

// GetSettlement consulta uma liquidação.
func (h *Handler) GetSettlement(ctx context.Context, req *spiv1.GetSettlementRequest) (*spiv1.GetSettlementResponse, error) {
	id, err := settlement.ParseID(req.GetSettlementId())
	if err != nil {
		return nil, err
	}
	st, err := h.svc.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &spiv1.GetSettlementResponse{Settlement: settlementToProto(st)}, nil
}

// SetBehavior troca o comportamento do simulador.
func (h *Handler) SetBehavior(_ context.Context, req *spiv1.SetBehaviorRequest) (*spiv1.SetBehaviorResponse, error) {
	b := req.GetBehavior()
	mode, ok := modeFromProto(b.GetSettleMode())
	if !ok {
		return nil, invalid("behavior.settle_mode", "must be a known settle mode")
	}
	if b.GetLatency() != nil && b.GetLatency().CheckValid() != nil {
		return nil, invalid("behavior.latency", "is not a valid duration")
	}
	nb := settlement.Behavior{Mode: mode, FailStatusQueries: b.GetFailStatusQueries(), Latency: b.GetLatency().AsDuration()}
	if err := h.behavior.Set(nb); err != nil {
		return nil, invalid("behavior", err.Error())
	}
	return &spiv1.SetBehaviorResponse{Behavior: behaviorToProto(h.behavior.Current())}, nil
}

// GetBehavior devolve o comportamento em vigor.
func (h *Handler) GetBehavior(context.Context, *spiv1.GetBehaviorRequest) (*spiv1.GetBehaviorResponse, error) {
	return &spiv1.GetBehaviorResponse{Behavior: behaviorToProto(h.behavior.Current())}, nil
}

var modes = map[spiv1.SettleMode]settlement.Mode{
	spiv1.SettleMode_SETTLE_MODE_SETTLE:                settlement.ModeSettle,
	spiv1.SettleMode_SETTLE_MODE_REJECT:                settlement.ModeReject,
	spiv1.SettleMode_SETTLE_MODE_SETTLE_NO_RESPONSE:    settlement.ModeSettleNoResponse,
	spiv1.SettleMode_SETTLE_MODE_NO_SETTLE_NO_RESPONSE: settlement.ModeNoSettleNoResponse,
}

func modeFromProto(m spiv1.SettleMode) (settlement.Mode, bool) {
	mode, ok := modes[m]
	return mode, ok
}

func modeToProto(m settlement.Mode) spiv1.SettleMode {
	for p, d := range modes {
		if d == m {
			return p
		}
	}
	return spiv1.SettleMode_SETTLE_MODE_UNSPECIFIED
}

func behaviorToProto(b settlement.Behavior) *spiv1.Behavior {
	return &spiv1.Behavior{
		SettleMode: modeToProto(b.Mode), FailStatusQueries: b.FailStatusQueries, Latency: durationpb.New(b.Latency),
	}
}

func settlementToProto(s settlement.Settlement) *spiv1.Settlement {
	out := &spiv1.Settlement{
		SettlementId: s.ID.String(), TransferId: s.TransferID.String(),
		PayerAccountId: s.Payer.String(), PayeeAccountId: s.Payee.String(),
		AmountCents: s.AmountCents, DecidedAt: timestamppb.New(s.DecidedAt),
	}
	switch s.Status {
	case settlement.Settled:
		out.Status = spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED
	case settlement.Rejected:
		out.Status = spiv1.SettlementStatus_SETTLEMENT_STATUS_REJECTED
		out.RejectReason = spiv1.RejectReason_REJECT_REASON_SIMULATED
	}
	return out
}

func invalid(field, problem string) error {
	return withReason(codes.InvalidArgument, field+": "+problem, spiv1.ErrorReason_ERROR_REASON_INVALID_REQUEST,
		map[string]string{"field": field}).Err()
}

func withReason(code codes.Code, msg string, reason spiv1.ErrorReason, md map[string]string) *status.Status {
	st := status.New(code, msg)
	out, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason.String(), Domain: errorDomain, Metadata: md})
	if err != nil {
		return st
	}
	return out
}

// statusFor traduz um erro para o status devolvido; falso quando o erro não é esperado.
func statusFor(err error) (*status.Status, bool) {
	var grpcErr interface{ GRPCStatus() *status.Status }
	var verr *settlement.ValidationError
	switch {
	case errors.As(err, &grpcErr):
		return grpcErr.GRPCStatus(), true
	case errors.As(err, &verr):
		return withReason(codes.InvalidArgument, verr.Error(), spiv1.ErrorReason_ERROR_REASON_INVALID_REQUEST,
			map[string]string{"field": verr.Field}), true
	case errors.Is(err, settlement.ErrNotFound):
		return withReason(codes.NotFound, "liquidação não encontrada", spiv1.ErrorReason_ERROR_REASON_SETTLEMENT_NOT_FOUND, nil), true
	case errors.Is(err, settlement.ErrIDReused):
		return withReason(codes.AlreadyExists, "settlement_id já usado com outro pedido",
			spiv1.ErrorReason_ERROR_REASON_SETTLEMENT_ID_REUSED, nil), true
	case errors.Is(err, settlement.ErrStatusUnavailable), errors.Is(err, settlement.ErrNoResponse):
		// Falhas simuladas: para o cliente, o SPI está indisponível.
		return status.New(codes.Unavailable, "SPI indisponível (simulado)"), true
	case errors.Is(err, context.DeadlineExceeded):
		return status.New(codes.DeadlineExceeded, "prazo da chamada esgotado"), true
	case errors.Is(err, context.Canceled):
		return status.New(codes.Canceled, "chamada cancelada"), true
	default:
		return status.New(codes.Internal, "erro interno"), false
	}
}

// ErrorInterceptor é o tratamento central de erros; erros inesperados vão para o log com a causa.
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
