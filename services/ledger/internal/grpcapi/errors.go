package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
)

// errorDomain vai em todo google.rpc.ErrorInfo do ledger (ADR-0004, seção 5).
const errorDomain = "ledger.conta.local"

// statusFor traduz um erro para o status gRPC devolvido ao cliente. É a única tradução do
// serviço: usada pelo interceptor na resposta e pelo Codec ao gravar uma recusa, por isso a
// repetição idempotente sai idêntica à original. O segundo retorno é falso quando o erro não
// é esperado: o cliente recebe só "erro interno" e a causa vai para o log.
func statusFor(err error) (*status.Status, bool) {
	// Já é um status: a recusa repetida (Codec.DecodeRejection) ou um erro do próprio gRPC.
	var grpcErr interface{ GRPCStatus() *status.Status }
	if errors.As(err, &grpcErr) {
		return grpcErr.GRPCStatus(), true
	}

	var verr *account.ValidationError
	var retryable interface{ Retryable() bool }
	switch {
	case errors.As(err, &verr):
		return withReason(codes.InvalidArgument, verr.Error(), ledgerv1.ErrorReason_ERROR_REASON_INVALID_REQUEST,
			map[string]string{"field": verr.Field}), true
	case errors.Is(err, account.ErrAccountNotFound):
		return withReason(codes.NotFound, "conta não encontrada", ledgerv1.ErrorReason_ERROR_REASON_ACCOUNT_NOT_FOUND, nil), true
	case errors.Is(err, account.ErrInsufficientFunds):
		return withReason(codes.FailedPrecondition, "saldo insuficiente", ledgerv1.ErrorReason_ERROR_REASON_INSUFFICIENT_FUNDS, nil), true
	case errors.Is(err, account.ErrAccountAlreadyExists):
		return withReason(codes.AlreadyExists, "o documento já tem conta", ledgerv1.ErrorReason_ERROR_REASON_ACCOUNT_ALREADY_EXISTS, nil), true
	case errors.Is(err, account.ErrEntryNotFound):
		return withReason(codes.NotFound, "lançamento não encontrado", ledgerv1.ErrorReason_ERROR_REASON_ENTRY_NOT_FOUND, nil), true
	case errors.Is(err, account.ErrEntryAlreadyReversed):
		return withReason(codes.FailedPrecondition, "lançamento já estornado",
			ledgerv1.ErrorReason_ERROR_REASON_ENTRY_ALREADY_REVERSED, nil), true
	case errors.Is(err, account.ErrEntryNotReversible):
		return withReason(codes.FailedPrecondition, "lançamento não pode ser estornado",
			ledgerv1.ErrorReason_ERROR_REASON_ENTRY_NOT_REVERSIBLE, nil), true
	case errors.Is(err, account.ErrIdempotencyKeyReused):
		return withReason(codes.AlreadyExists, "chave de idempotência já usada com outro pedido",
			ledgerv1.ErrorReason_ERROR_REASON_IDEMPOTENCY_KEY_REUSED, nil), true
	case errors.As(err, &retryable) && retryable.Retryable():
		// Conta travada além do lock_timeout, deadlock: repetir com a mesma chave é seguro.
		return status.New(codes.Unavailable, "conta ocupada, tente novamente"), true
	case errors.Is(err, context.DeadlineExceeded):
		return status.New(codes.DeadlineExceeded, "prazo da chamada esgotado"), true
	case errors.Is(err, context.Canceled):
		return status.New(codes.Canceled, "chamada cancelada"), true
	default:
		return status.New(codes.Internal, "erro interno"), false
	}
}

func withReason(code codes.Code, msg string, reason ledgerv1.ErrorReason, metadata map[string]string) *status.Status {
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{
		Reason: reason.String(), Domain: errorDomain, Metadata: metadata,
	})
	if err != nil {
		// Só falha se o detalhe não puder ser serializado, o que não acontece com ErrorInfo.
		return status.New(code, msg)
	}
	return st
}

// ErrorInterceptor é o tratamento central de erros: os handlers devolvem erros de domínio e
// ele os traduz para status gRPC. Erros inesperados viram "erro interno" para o cliente e são
// registrados aqui, com a causa, no log ligado ao trace.
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
