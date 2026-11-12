package grpcapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/grpcapi"
	"conta.local/ledger/internal/platform/logging"
	"conta.local/ledger/internal/platform/postgres/pgtest"
	"conta.local/ledger/internal/postgres"
)

// ledgerClient sobe o servidor gRPC real (interceptors incluídos) sobre svc, em memória.
func ledgerClient(t *testing.T, svc grpcapi.LedgerService, logs *bytes.Buffer) ledgerv1.LedgerServiceClient {
	t.Helper()
	if logs == nil {
		logs = &bytes.Buffer{}
	}
	srv := grpcapi.NewServer(grpcapi.ServerOptions{
		Logger:         logging.New(logs, slog.LevelDebug, nil),
		TracerProvider: tracenoop.NewTracerProvider(),
		MeterProvider:  noop.NewMeterProvider(),
		Propagator:     propagation.TraceContext{},
	})
	grpcapi.RegisterLedger(srv, grpcapi.NewLedgerHandler(svc))
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
	return ledgerv1.NewLedgerServiceClient(conn)
}

// realLedger monta a pilha inteira: gRPC -> handler -> casos de uso -> Postgres.
func realLedger(t *testing.T) ledgerv1.LedgerServiceClient {
	t.Helper()
	_, pool := pgtest.Migrated(t, 0)
	return ledgerClient(t, account.NewService(postgres.NewStore(pool), grpcapi.Codec{}), nil)
}

var keys atomic.Int64

func key() string { return fmt.Sprintf("k-%d-%d", time.Now().UnixNano(), keys.Add(1)) }

// openAccount abre a conta de teste, com o CPF 111.444.777-35.
func openAccount(t *testing.T, c ledgerv1.LedgerServiceClient) string {
	t.Helper()
	resp, err := c.OpenAccount(t.Context(), &ledgerv1.OpenAccountRequest{
		IdempotencyKey: key(), HolderName: "Maria Silva", HolderDocument: "11144477735",
	})
	if err != nil {
		t.Fatalf("OpenAccount: %v", err)
	}
	return resp.GetAccount().GetAccountId()
}

func credit(t *testing.T, c ledgerv1.LedgerServiceClient, id string, cents int64) {
	t.Helper()
	_, err := c.Credit(t.Context(), &ledgerv1.CreditRequest{
		IdempotencyKey: key(), AccountId: id, AmountCents: cents, Reason: ledgerv1.CreditReason_CREDIT_REASON_OPENING,
	})
	if err != nil {
		t.Fatalf("Credit: %v", err)
	}
}

func reasonOf(err error) (codes.Code, string, map[string]string) {
	st := status.Convert(err)
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return st.Code(), info.GetReason(), info.GetMetadata()
		}
	}
	return st.Code(), "", nil
}

func TestLedgerOverGRPC(t *testing.T) {
	c := realLedger(t)
	ctx := t.Context()
	id := openAccount(t, c)

	acc, err := c.GetAccount(ctx, &ledgerv1.GetAccountRequest{AccountId: id})
	if err != nil || acc.GetAccount().GetStatus() != ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE ||
		acc.GetAccount().GetHolderDocument() != "11144477735" {
		t.Fatalf("GetAccount = %v, %v", acc, err)
	}
	credit(t, c, id, 10_000)
	debit, err := c.Debit(ctx, &ledgerv1.DebitRequest{
		IdempotencyKey: key(), AccountId: id, AmountCents: 2_500,
		Reason: ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL, Description: "Pix para João",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := debit.GetPosting()
	if p.GetDirection() != ledgerv1.Direction_DIRECTION_DEBIT || p.GetBalanceAfterCents() != 7_500 || p.GetDescription() != "Pix para João" {
		t.Fatalf("posting = %v", p)
	}
	bal, err := c.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: id})
	if err != nil || bal.GetBalanceCents() != 7_500 || bal.GetAccountId() != id {
		t.Fatalf("GetBalance = %v, %v", bal, err)
	}

	// Extrato página a página, seguindo o next_page_token.
	var seen []int64
	token := ""
	for range 3 {
		page, err := c.ListStatement(ctx, &ledgerv1.ListStatementRequest{AccountId: id, PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page.GetPostings() {
			seen = append(seen, p.GetBalanceAfterCents())
		}
		token = page.GetNextPageToken()
		if token == "" {
			break
		}
	}
	if len(seen) != 2 || seen[0] != 7_500 || seen[1] != 10_000 || token != "" {
		t.Fatalf("extrato = %v (token final %q), want [7500 10000] e sem próxima página", seen, token)
	}
}

// A recusa repetida pela mesma chave é idêntica à original: mesmo código, mesma mensagem e
// mesmos detalhes (ErrorInfo), mesmo depois de o saldo passar a cobrir o débito.
func TestRejectionReplayIsIdentical(t *testing.T) {
	c := realLedger(t)
	id := openAccount(t, c)
	credit(t, c, id, 100)
	req := &ledgerv1.DebitRequest{
		IdempotencyKey: key(), AccountId: id, AmountCents: 500, Reason: ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL,
	}

	_, first := c.Debit(t.Context(), req)
	credit(t, c, id, 1_000)
	_, again := c.Debit(t.Context(), req)

	if code, reason, _ := reasonOf(first); code != codes.FailedPrecondition || reason != "ERROR_REASON_INSUFFICIENT_FUNDS" {
		t.Fatalf("primeira chamada = %v", first)
	}
	if !proto.Equal(status.Convert(first).Proto(), status.Convert(again).Proto()) {
		t.Fatalf("repetição diferente da original:\n  original:  %v\n  repetição: %v", status.Convert(first).Proto(), status.Convert(again).Proto())
	}
}

func TestSuccessReplayIsIdentical(t *testing.T) {
	c := realLedger(t)
	id := openAccount(t, c)
	credit(t, c, id, 1_000)
	req := &ledgerv1.DebitRequest{
		IdempotencyKey: key(), AccountId: id, AmountCents: 100, Reason: ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL,
	}
	first, err := c.Debit(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Debit(t.Context(), req)
	if err != nil || !proto.Equal(first, again) {
		t.Fatalf("repetição = %v, %v; original = %v", again, err, first)
	}
}

func TestDomainErrorsOverGRPC(t *testing.T) {
	c := realLedger(t)
	ctx := t.Context()
	id := openAccount(t, c)
	missing := "0195d1f2-0000-7000-8000-000000000999"
	reused := key()
	if _, err := c.Credit(ctx, &ledgerv1.CreditRequest{IdempotencyKey: reused, AccountId: id, AmountCents: 1,
		Reason: ledgerv1.CreditReason_CREDIT_REASON_OPENING}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		call       func() error
		wantCode   codes.Code
		wantReason string
	}{
		{"conta inexistente no saldo", func() error {
			_, err := c.GetBalance(ctx, &ledgerv1.GetBalanceRequest{AccountId: missing})
			return err
		}, codes.NotFound, "ERROR_REASON_ACCOUNT_NOT_FOUND"},
		{"conta inexistente no débito", func() error {
			_, err := c.Debit(ctx, &ledgerv1.DebitRequest{IdempotencyKey: key(), AccountId: missing, AmountCents: 1,
				Reason: ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL})
			return err
		}, codes.NotFound, "ERROR_REASON_ACCOUNT_NOT_FOUND"},
		{"CPF já tem conta", func() error {
			_, err := c.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{IdempotencyKey: key(), HolderName: "Outra", HolderDocument: "11144477735"})
			return err
		}, codes.AlreadyExists, "ERROR_REASON_ACCOUNT_ALREADY_EXISTS"},
		{"chave reutilizada com outro valor", func() error {
			_, err := c.Credit(ctx, &ledgerv1.CreditRequest{IdempotencyKey: reused, AccountId: id, AmountCents: 2,
				Reason: ledgerv1.CreditReason_CREDIT_REASON_OPENING})
			return err
		}, codes.AlreadyExists, "ERROR_REASON_IDEMPOTENCY_KEY_REUSED"},
		{"extrato de conta inexistente", func() error {
			_, err := c.ListStatement(ctx, &ledgerv1.ListStatementRequest{AccountId: missing})
			return err
		}, codes.NotFound, "ERROR_REASON_ACCOUNT_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, reason, _ := reasonOf(tt.call()); code != tt.wantCode || reason != tt.wantReason {
				t.Fatalf("= %s %s, want %s %s", code, reason, tt.wantCode, tt.wantReason)
			}
		})
	}
}

// fakeLedger responde a todas as chamadas com o mesmo erro; para testar a borda sozinha.
type fakeLedger struct{ err error }

func (f fakeLedger) OpenAccount(context.Context, account.OpenAccountCommand) (account.Account, error) {
	return account.Account{}, f.err
}
func (f fakeLedger) Credit(context.Context, account.CreditCommand) (account.Posting, error) {
	return account.Posting{}, f.err
}
func (f fakeLedger) Debit(context.Context, account.DebitCommand) (account.Posting, error) {
	return account.Posting{}, f.err
}
func (f fakeLedger) Account(context.Context, account.ID) (account.Account, error) {
	return account.Account{}, f.err
}
func (f fakeLedger) Balance(context.Context, account.ID) (account.Money, error) {
	return account.Money{}, f.err
}
func (f fakeLedger) Statement(context.Context, account.ID, int64, int) (account.StatementPage, error) {
	return account.StatementPage{}, f.err
}

// Pedidos inválidos são recusados na borda, antes do caso de uso, com o campo culpado.
func TestValidationOverGRPC(t *testing.T) {
	c := ledgerClient(t, fakeLedger{err: errors.New("não deveria ser chamado")}, nil)
	ctx := t.Context()
	id := "0195d1f2-0000-7000-8000-000000000001"
	validDebit := func() *ledgerv1.DebitRequest {
		return &ledgerv1.DebitRequest{IdempotencyKey: "k", AccountId: id, AmountCents: 1, Reason: ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL}
	}
	tests := []struct {
		name      string
		call      func() error
		wantField string
	}{
		{"sem chave", func() error { r := validDebit(); r.IdempotencyKey = ""; _, err := c.Debit(ctx, r); return err }, "idempotency_key"},
		{"conta não é UUID", func() error { r := validDebit(); r.AccountId = "abc"; _, err := c.Debit(ctx, r); return err }, "account_id"},
		{"valor zero", func() error { r := validDebit(); r.AmountCents = 0; _, err := c.Debit(ctx, r); return err }, "amount_cents"},
		{"valor negativo", func() error { r := validDebit(); r.AmountCents = -5; _, err := c.Debit(ctx, r); return err }, "amount_cents"},
		{"motivo não informado", func() error { r := validDebit(); r.Reason = 0; _, err := c.Debit(ctx, r); return err }, "reason"},
		{"motivo de crédito desconhecido", func() error {
			_, err := c.Credit(ctx, &ledgerv1.CreditRequest{IdempotencyKey: "k", AccountId: id, AmountCents: 1, Reason: 99})
			return err
		}, "reason"},
		{"CPF inválido", func() error {
			_, err := c.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{IdempotencyKey: "k", HolderName: "A", HolderDocument: "123"})
			return err
		}, "holder_document"},
		{"sem nome", func() error {
			_, err := c.OpenAccount(ctx, &ledgerv1.OpenAccountRequest{IdempotencyKey: "k", HolderDocument: "11144477735"})
			return err
		}, "holder_name"},
		{"page_token forjado", func() error {
			_, err := c.ListStatement(ctx, &ledgerv1.ListStatementRequest{AccountId: id, PageToken: "abc"})
			return err
		}, "page_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, reason, md := reasonOf(tt.call())
			if code != codes.InvalidArgument || reason != "ERROR_REASON_INVALID_REQUEST" || md["field"] != tt.wantField {
				t.Fatalf("= %s %s %v, want INVALID_ARGUMENT no campo %s", code, reason, md, tt.wantField)
			}
		})
	}
}

// Um erro inesperado chega ao cliente só como "erro interno"; a causa fica no log.
func TestInternalErrorIsLoggedNotLeaked(t *testing.T) {
	var logs bytes.Buffer
	c := ledgerClient(t, fakeLedger{err: errors.New("pgx: senha do banco no erro")}, &logs)

	_, err := c.GetBalance(t.Context(), &ledgerv1.GetBalanceRequest{AccountId: "0195d1f2-0000-7000-8000-000000000001"})
	st := status.Convert(err)
	if st.Code() != codes.Internal || st.Message() != "erro interno" {
		t.Fatalf("cliente recebeu %s %q", st.Code(), st.Message())
	}
	if !strings.Contains(logs.String(), "pgx: senha do banco no erro") || !strings.Contains(logs.String(), `"code":"Internal"`) {
		t.Fatalf("a causa e o código deveriam estar no log:\n%s", logs.String())
	}
}
