package main

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/grpcapi"
	"conta.local/ledger/internal/platform/postgres/pgtest"
	"conta.local/ledger/internal/postgres"
	"conta.local/platform/grpcserver"
	"conta.local/platform/logging"
)

// ledger sobe a pilha inteira (gRPC -> casos de uso -> Postgres de teste) numa porta TCP local
// e devolve um cliente e o endereço, para testar também o run de ponta a ponta.
func ledger(t *testing.T) (ledgerv1.LedgerServiceClient, string) {
	t.Helper()
	_, pool := pgtest.Migrated(t, 0)
	logger := logging.New(&bytes.Buffer{}, slog.LevelInfo, nil)
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger:         logger,
		Interceptors:   []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
		TracerProvider: tracenoop.NewTracerProvider(),
		MeterProvider:  noop.NewMeterProvider(),
		Propagator:     propagation.TraceContext{},
	})
	grpcapi.RegisterLedger(srv, grpcapi.NewLedgerHandler(account.NewService(postgres.NewStore(pool), grpcapi.Codec{})))
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return ledgerv1.NewLedgerServiceClient(conn), lis.Addr().String()
}

func TestDemoAccountsFileIsValid(t *testing.T) {
	accounts, err := parseAccounts(demoAccountsJSON)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var zero bool
	for _, a := range accounts {
		if _, err := account.ParseCPF(a.HolderDocument); err != nil {
			t.Errorf("%s: CPF inválido: %v", a.HolderName, err)
		}
		if _, err := account.NewHolderName(a.HolderName); err != nil {
			t.Errorf("%s: nome inválido: %v", a.HolderName, err)
		}
		if seen[a.HolderDocument] {
			t.Errorf("CPF repetido no arquivo: %s", a.HolderDocument)
		}
		seen[a.HolderDocument] = true
		if a.OpeningBalanceCents < 0 {
			t.Errorf("%s: saldo inicial negativo", a.HolderName)
		}
		zero = zero || a.OpeningBalanceCents == 0
	}
	if !zero {
		t.Error("deve haver uma conta sem saldo, para demonstrar a recusa por saldo insuficiente")
	}
	if _, err := parseAccounts([]byte("[]")); err == nil {
		t.Error("arquivo vazio deveria ser recusado")
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	c, _ := ledger(t)
	accounts, err := parseAccounts(demoAccountsJSON)
	if err != nil {
		t.Fatal(err)
	}

	first, err := Seed(t.Context(), c, accounts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Seed(t.Context(), c, accounts)
	if err != nil {
		t.Fatalf("segunda execução: %v", err)
	}
	for i, a := range accounts {
		if first[i].AccountID != second[i].AccountID {
			t.Errorf("%s: a segunda execução criou outra conta (%s, %s)", a.HolderName, first[i].AccountID, second[i].AccountID)
		}
		if second[i].BalanceCents != a.OpeningBalanceCents {
			t.Errorf("%s: saldo %d, want %d (sem crédito em dobro)", a.HolderName, second[i].BalanceCents, a.OpeningBalanceCents)
		}
		page, err := c.ListStatement(t.Context(), &ledgerv1.ListStatementRequest{AccountId: first[i].AccountID})
		if err != nil {
			t.Fatal(err)
		}
		wantLines := 0
		if a.OpeningBalanceCents > 0 {
			wantLines = 1
		}
		if len(page.GetPostings()) != wantLines {
			t.Errorf("%s: %d linhas no extrato, want %d", a.HolderName, len(page.GetPostings()), wantLines)
		}
	}
}

// Mudar o saldo inicial no arquivo sem subir a versão da chave não altera a conta: o
// ledger recusa a chave reutilizada com outro pedido, e o seed falha alto.
func TestSeedRefusesToChangeAnExistingBalance(t *testing.T) {
	c, _ := ledger(t)
	accounts := []DemoAccount{{HolderName: "Ana Demonstração", HolderDocument: "12345678909", OpeningBalanceCents: 1_000}}
	if _, err := Seed(t.Context(), c, accounts); err != nil {
		t.Fatal(err)
	}
	accounts[0].OpeningBalanceCents = 2_000
	_, err := Seed(t.Context(), c, accounts)
	if status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), "saldo inicial") {
		t.Fatalf("err = %v, want ALREADY_EXISTS (chave reutilizada) no saldo inicial", err)
	}
}

func TestRunEndToEnd(t *testing.T) {
	_, target := ledger(t)
	env := func(k string) (string, bool) {
		if k == "LEDGER_GRPC_TARGET" {
			return target, true
		}
		return "", false
	}
	var out bytes.Buffer
	for range 2 {
		if err := run(t.Context(), env, &out); err != nil {
			t.Fatalf("run: %v\n%s", err, out.String())
		}
	}
	if got := strings.Count(out.String(), "seed concluído"); got != 2 {
		t.Fatalf("%d execuções concluídas no log, want 2:\n%s", got, out.String())
	}
	accounts, _ := parseAccounts(demoAccountsJSON)
	for _, a := range accounts {
		if strings.Contains(out.String(), a.HolderDocument) {
			t.Errorf("o log contém o CPF completo %s", a.HolderDocument)
		}
	}
	if !strings.Contains(out.String(), "***.456.789-**") {
		t.Errorf("o log deveria trazer o CPF mascarado:\n%s", out.String())
	}
}
