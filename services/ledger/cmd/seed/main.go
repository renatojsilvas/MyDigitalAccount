// Command seed cria as contas de demonstração usadas nas fases seguintes, como um cliente
// qualquer da API gRPC do ledger: passa pela validação, pela partida dobrada e pela
// idempotência. Rodar de novo não duplica nada (chaves fixas e versionadas).
//
// Uso (com o ledger no ar): docker compose run --rm tools make seed
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
	"conta.local/platform/logging"
)

// keyVersion entra em toda chave do seed. Mudou um saldo inicial no arquivo? Suba a versão:
// com a chave antiga, o ledger recusaria o novo valor (IDEMPOTENCY_KEY_REUSED), e o seed
// nunca "corrige" um saldo existente.
const keyVersion = "seed-v1"

const callTimeout = 5 * time.Second

//go:embed demo-accounts.json
var demoAccountsJSON []byte

// DemoAccount é uma conta de demonstração do arquivo.
type DemoAccount struct {
	HolderName          string `json:"holder_name"`
	HolderDocument      string `json:"holder_document"`
	OpeningBalanceCents int64  `json:"opening_balance_cents"`
}

// Result é o que o seed garantiu para uma conta.
type Result struct {
	AccountID    string
	Document     account.Document
	BalanceCents int64
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.LookupEnv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		stop()
		os.Exit(1)
	}
}

func run(ctx context.Context, lookup func(string) (string, bool), stdout io.Writer) error {
	target, ok := lookup("LEDGER_GRPC_TARGET")
	if !ok || target == "" {
		target = "ledger:50051"
	}
	logger := logging.New(stdout, slog.LevelInfo, nil)

	accounts, err := parseAccounts(demoAccountsJSON)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("cliente gRPC para %s: %w", target, err)
	}
	defer func() { _ = conn.Close() }()

	results, err := Seed(ctx, ledgerv1.NewLedgerServiceClient(conn), accounts)
	if err != nil {
		return err
	}
	for _, r := range results {
		// CPF sempre mascarado em log (o tipo Document já cuida disso).
		logger.InfoContext(ctx, "conta de demonstração pronta",
			"account_id", r.AccountID, "holder_document", r.Document, "balance_cents", r.BalanceCents)
	}
	logger.InfoContext(ctx, "seed concluído", "accounts", len(results), "target", target)
	return nil
}

func parseAccounts(data []byte) ([]DemoAccount, error) {
	var accounts []DemoAccount
	if err := json.Unmarshal(data, &accounts); err != nil {
		return nil, fmt.Errorf("arquivo de contas: %w", err)
	}
	if len(accounts) == 0 {
		return nil, errors.New("arquivo de contas vazio")
	}
	return accounts, nil
}

// Seed garante cada conta (abertura e saldo inicial) e devolve o estado final. Cada chamada
// usa uma chave derivada do CPF, então repetir o seed devolve as mesmas contas.
func Seed(ctx context.Context, c ledgerv1.LedgerServiceClient, accounts []DemoAccount) ([]Result, error) {
	results := make([]Result, 0, len(accounts))
	for _, a := range accounts {
		doc, err := account.ParseCPF(a.HolderDocument)
		if err != nil {
			return nil, fmt.Errorf("conta %q: %w", a.HolderName, err)
		}
		r, err := seedOne(ctx, c, a, doc)
		if err != nil {
			return nil, fmt.Errorf("conta %s: %w", doc, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func seedOne(ctx context.Context, c ledgerv1.LedgerServiceClient, a DemoAccount, doc account.Document) (Result, error) {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	opened, err := c.OpenAccount(callCtx, &ledgerv1.OpenAccountRequest{
		IdempotencyKey: keyVersion + ":open:" + doc.Digits(),
		HolderName:     a.HolderName,
		HolderDocument: doc.Digits(),
	})
	if err != nil {
		return Result{}, fmt.Errorf("abrindo: %w", err)
	}
	id := opened.GetAccount().GetAccountId()

	if a.OpeningBalanceCents > 0 {
		creditCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		_, err := c.Credit(creditCtx, &ledgerv1.CreditRequest{
			IdempotencyKey: keyVersion + ":opening:" + doc.Digits(),
			AccountId:      id,
			AmountCents:    a.OpeningBalanceCents,
			Reason:         ledgerv1.CreditReason_CREDIT_REASON_OPENING,
			Description:    "Saldo inicial (demonstração)",
		})
		if err != nil {
			return Result{}, fmt.Errorf("creditando o saldo inicial: %w", err)
		}
	}

	balCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	bal, err := c.GetBalance(balCtx, &ledgerv1.GetBalanceRequest{AccountId: id})
	if err != nil {
		return Result{}, fmt.Errorf("lendo o saldo: %w", err)
	}
	return Result{AccountID: id, Document: doc, BalanceCents: bal.GetBalanceCents()}, nil
}
