// Package grpcapi é a borda gRPC do transfers: converte pedidos e respostas do contrato
// (conta.transfers.v1) e traduz erros de domínio para status gRPC com google.rpc.ErrorInfo. É
// a única camada do serviço que conhece gRPC.
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
	"google.golang.org/protobuf/types/known/timestamppb"

	transfersv1 "conta.local/gen/go/conta/transfers/v1"
	"conta.local/transfers/internal/pix"
)

const errorDomain = "transfers.conta.local"

// Transfers são os casos de uso que a borda chama, definidos aqui, por quem consome.
type Transfers interface {
	Start(ctx context.Context, cmd pix.StartCommand) (pix.Transfer, error)
	Get(ctx context.Context, accountID, id uuid.UUID) (pix.Transfer, []pix.Change, error)
	Limits(ctx context.Context, accountID uuid.UUID) (pix.Limits, error)
}

// TraceCarrier extrai o contexto de trace da requisição como carrier W3C, para a saga continuar
// o mesmo trace (ADR-0007, seção 4). Vem do main, com o propagador da telemetria.
type TraceCarrier func(ctx context.Context) map[string]string

// Handler implementa TransfersService.
type Handler struct {
	transfersv1.UnimplementedTransfersServiceServer
	svc   Transfers
	trace TraceCarrier
}

// NewHandler cria o handler.
func NewHandler(svc Transfers, trace TraceCarrier) *Handler {
	return &Handler{svc: svc, trace: trace}
}

// Register registra o serviço no servidor.
func Register(srv grpc.ServiceRegistrar, h *Handler) {
	transfersv1.RegisterTransfersServiceServer(srv, h)
}

// StartPix aceita um Pix (202 no BFF): só valida o formato; o resto é da saga.
func (h *Handler) StartPix(ctx context.Context, req *transfersv1.StartPixRequest) (*transfersv1.StartPixResponse, error) {
	keyType, ok := keyTypes[req.GetDestinationKey().GetType()]
	if !ok {
		return nil, &pix.ValidationError{Field: "destination_key.type", Problem: "must be a known key type"}
	}
	cmd, err := pix.NewStartCommand(req.GetIdempotencyKey(), req.GetAccountId(), keyType,
		req.GetDestinationKey().GetValue(), req.GetAmountCents(), req.GetDescription())
	if err != nil {
		return nil, err
	}
	cmd.Trace = h.trace(ctx)
	t, err := h.svc.Start(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return &transfersv1.StartPixResponse{Pix: pixToProto(t, nil)}, nil
}

// GetPix devolve o Pix da conta, com o histórico.
func (h *Handler) GetPix(ctx context.Context, req *transfersv1.GetPixRequest) (*transfersv1.GetPixResponse, error) {
	account, err := pix.ParseAccountID("account_id", req.GetAccountId())
	if err != nil {
		return nil, err
	}
	id, err := pix.ParseAccountID("pix_id", req.GetPixId())
	if err != nil {
		return nil, err
	}
	t, history, err := h.svc.Get(ctx, account, id)
	if err != nil {
		return nil, err
	}
	return &transfersv1.GetPixResponse{Pix: pixToProto(t, history)}, nil
}

// GetLimits devolve o limite diário da conta.
func (h *Handler) GetLimits(ctx context.Context, req *transfersv1.GetLimitsRequest) (*transfersv1.GetLimitsResponse, error) {
	account, err := pix.ParseAccountID("account_id", req.GetAccountId())
	if err != nil {
		return nil, err
	}
	l, err := h.svc.Limits(ctx, account)
	if err != nil {
		return nil, err
	}
	return &transfersv1.GetLimitsResponse{
		AccountId: account.String(), DailyLimitCents: l.DailyCents, UsedTodayCents: l.UsedCents,
		AvailableTodayCents: l.AvailableCents, ResetsAt: timestamppb.New(l.ResetsAt),
	}, nil
}

var keyTypes = map[transfersv1.PixKeyType]pix.KeyType{transfersv1.PixKeyType_PIX_KEY_TYPE_CPF: pix.KeyCPF}

var states = map[pix.State]transfersv1.PixState{
	pix.StateAccepted: transfersv1.PixState_PIX_STATE_ACCEPTED, pix.StateValidated: transfersv1.PixState_PIX_STATE_VALIDATED,
	pix.StateDebited: transfersv1.PixState_PIX_STATE_DEBITED, pix.StateSettling: transfersv1.PixState_PIX_STATE_SETTLING,
	pix.StatePendingConfirmation: transfersv1.PixState_PIX_STATE_PENDING_CONFIRMATION,
	pix.StateSettled:             transfersv1.PixState_PIX_STATE_SETTLED, pix.StateCompleted: transfersv1.PixState_PIX_STATE_COMPLETED,
	pix.StateReversing: transfersv1.PixState_PIX_STATE_REVERSING, pix.StateReversed: transfersv1.PixState_PIX_STATE_REVERSED,
	pix.StateRejected: transfersv1.PixState_PIX_STATE_REJECTED,
}

var statuses = map[pix.Status]transfersv1.PixStatus{
	pix.StatusProcessing: transfersv1.PixStatus_PIX_STATUS_PROCESSING, pix.StatusCompleted: transfersv1.PixStatus_PIX_STATUS_COMPLETED,
	pix.StatusFailed: transfersv1.PixStatus_PIX_STATUS_FAILED, pix.StatusRejected: transfersv1.PixStatus_PIX_STATUS_REJECTED,
}

var reasons = map[pix.Reason]transfersv1.PixReason{
	pix.ReasonAntifraudDenied:       transfersv1.PixReason_PIX_REASON_ANTIFRAUD_DENIED,
	pix.ReasonLimitExceeded:         transfersv1.PixReason_PIX_REASON_LIMIT_EXCEEDED,
	pix.ReasonDestinationInvalid:    transfersv1.PixReason_PIX_REASON_DESTINATION_INVALID,
	pix.ReasonSameAccount:           transfersv1.PixReason_PIX_REASON_SAME_ACCOUNT,
	pix.ReasonInsufficientFunds:     transfersv1.PixReason_PIX_REASON_INSUFFICIENT_FUNDS,
	pix.ReasonDependencyUnavailable: transfersv1.PixReason_PIX_REASON_DEPENDENCY_UNAVAILABLE,
	pix.ReasonSettlementRejected:    transfersv1.PixReason_PIX_REASON_SETTLEMENT_REJECTED,
	pix.ReasonSettlementNotSent:     transfersv1.PixReason_PIX_REASON_SETTLEMENT_NOT_SENT,
	pix.ReasonSourceAccountInvalid:  transfersv1.PixReason_PIX_REASON_SOURCE_ACCOUNT_INVALID,
}

func idOrEmpty(id uuid.UUID) string {
	if id == (uuid.UUID{}) {
		return ""
	}
	return id.String()
}

// pixToProto monta a resposta. A chave de destino sai só mascarada.
func pixToProto(t pix.Transfer, history []pix.Change) *transfersv1.Pix {
	out := &transfersv1.Pix{
		PixId: t.ID.String(), SourceAccountId: t.SourceAccountID.String(),
		DestinationAccountId: idOrEmpty(t.DestinationAccountID),
		DestinationKeyMasked: t.DestinationKey.Masked(), DestinationKeyType: transfersv1.PixKeyType_PIX_KEY_TYPE_CPF,
		AmountCents: t.AmountCents, Description: t.Description,
		State: states[t.State], Status: statuses[t.State.Status()], Reason: reasons[t.Reason],
		ConfirmationOverdue: t.ConfirmationOverdue,
		CreatedAt:           timestamppb.New(t.CreatedAt), UpdatedAt: timestamppb.New(t.UpdatedAt),
	}
	for _, c := range history {
		out.History = append(out.History, &transfersv1.PixStateChange{
			FromState: states[c.From], ToState: states[c.To], Reason: reasons[c.Reason], At: timestamppb.New(c.At),
		})
	}
	return out
}

func withReason(code codes.Code, msg string, reason transfersv1.ErrorReason, md map[string]string) *status.Status {
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
	var verr *pix.ValidationError
	switch {
	case errors.As(err, &grpcErr):
		return grpcErr.GRPCStatus(), true
	case errors.As(err, &verr):
		return withReason(codes.InvalidArgument, verr.Error(), transfersv1.ErrorReason_ERROR_REASON_INVALID_REQUEST,
			map[string]string{"field": verr.Field}), true
	case errors.Is(err, pix.ErrNotFound):
		return withReason(codes.NotFound, "Pix não encontrado", transfersv1.ErrorReason_ERROR_REASON_PIX_NOT_FOUND, nil), true
	case errors.Is(err, pix.ErrIdempotencyKeyReused):
		return withReason(codes.AlreadyExists, "chave de idempotência já usada com outro pedido",
			transfersv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_KEY_REUSED, nil), true
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
