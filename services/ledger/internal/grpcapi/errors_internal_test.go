package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"conta.local/ledger/internal/account"
)

type retryableErr struct{}

func (retryableErr) Error() string   { return "lock timeout" }
func (retryableErr) Retryable() bool { return true }

func errorInfo(st *status.Status) *errdetails.ErrorInfo {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info
		}
	}
	return nil
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCode   codes.Code
		wantReason string // vazio: sem ErrorInfo
		wantField  string
		expected   bool
	}{
		{"validação", &account.ValidationError{Field: "amount_cents", Problem: "must be greater than zero"},
			codes.InvalidArgument, "ERROR_REASON_INVALID_REQUEST", "amount_cents", true},
		{"conta inexistente", account.ErrAccountNotFound, codes.NotFound, "ERROR_REASON_ACCOUNT_NOT_FOUND", "", true},
		{"saldo insuficiente embrulhado", fmt.Errorf("debit: %w", account.ErrInsufficientFunds),
			codes.FailedPrecondition, "ERROR_REASON_INSUFFICIENT_FUNDS", "", true},
		{"CPF já tem conta", account.ErrAccountAlreadyExists, codes.AlreadyExists, "ERROR_REASON_ACCOUNT_ALREADY_EXISTS", "", true},
		{"chave reutilizada", account.ErrIdempotencyKeyReused, codes.AlreadyExists, "ERROR_REASON_IDEMPOTENCY_KEY_REUSED", "", true},
		{"repetível", fmt.Errorf("travando: %w", retryableErr{}), codes.Unavailable, "", "", true},
		{"prazo", context.DeadlineExceeded, codes.DeadlineExceeded, "", "", true},
		{"cancelada", context.Canceled, codes.Canceled, "", "", true},
		{"status já pronto (recusa repetida)", status.Error(codes.NotFound, "x"), codes.NotFound, "", "", true},
		{"invariante quebrada", account.ErrInvalidEntry, codes.Internal, "", "", false},
		{"erro qualquer", errors.New("senha=segredo no driver"), codes.Internal, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, expected := statusFor(tt.err)
			if st.Code() != tt.wantCode || expected != tt.expected {
				t.Fatalf("= %s (esperado=%v), want %s (esperado=%v)", st.Code(), expected, tt.wantCode, tt.expected)
			}
			info := errorInfo(st)
			if tt.wantReason == "" {
				if info != nil {
					t.Fatalf("ErrorInfo inesperado: %v", info)
				}
			} else if info == nil || info.GetReason() != tt.wantReason || info.GetDomain() != errorDomain ||
				info.GetMetadata()["field"] != tt.wantField {
				t.Fatalf("ErrorInfo = %v, want reason %s, field %q", info, tt.wantReason, tt.wantField)
			}
			if !tt.expected && (st.Message() != "erro interno" || strings.Contains(st.Message(), "segredo")) {
				t.Fatalf("erro interno vazou para o cliente: %q", st.Message())
			}
		})
	}
}

func TestPageToken(t *testing.T) {
	for _, seq := range []int64{1, 42, 1 << 40} {
		got, err := decodePageToken(encodePageToken(seq))
		if err != nil || got != seq {
			t.Errorf("ida e volta de %d = %d, %v", seq, got, err)
		}
	}
	if encodePageToken(0) != "" {
		t.Error("sem próxima página o token deve ser vazio")
	}
	if got, err := decodePageToken(""); got != 0 || err != nil {
		t.Errorf("token vazio = %d, %v; want primeira página", got, err)
	}
	for _, bad := range []string{"%%%", "AQ", encodePageToken(5)[:4], "AgAAAAAAAAAB", "AQAAAAAAAAAA"} {
		var verr *account.ValidationError
		if _, err := decodePageToken(bad); !errors.As(err, &verr) || verr.Field != "page_token" {
			t.Errorf("token %q: err = %v, want ValidationError em page_token", bad, err)
		}
	}
}

func TestCodecRoundTrip(t *testing.T) {
	c := Codec{}
	if _, _, err := c.EncodeRejection(errors.New("não é negócio")); err == nil {
		t.Error("EncodeRejection deve recusar um erro que não é recusa de negócio")
	}
	code, body, err := c.EncodeRejection(account.ErrInsufficientFunds)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := status.FromError(c.DecodeRejection(code, body))
	if !ok || st.Code() != codes.FailedPrecondition || errorInfo(st).GetReason() != "ERROR_REASON_INSUFFICIENT_FUNDS" {
		t.Fatalf("recusa decodificada = %v", st)
	}
	if err := c.DecodeRejection(code+1, body); err == nil || status.Code(err) != codes.Unknown {
		t.Errorf("código divergente deveria ser erro comum (vira INTERNAL), veio %v", err)
	}
}
