package grpcapi

import (
	"context"

	"google.golang.org/grpc"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
)

// LedgerService são os casos de uso que a borda chama, definidos aqui, por quem consome.
// account.Service os implementa; os testes da borda usam um dublê.
type LedgerService interface {
	OpenAccount(ctx context.Context, cmd account.OpenAccountCommand) (account.Account, error)
	Credit(ctx context.Context, cmd account.CreditCommand) (account.Posting, error)
	Debit(ctx context.Context, cmd account.DebitCommand) (account.Posting, error)
	Account(ctx context.Context, id account.ID) (account.Account, error)
	AccountByDocument(ctx context.Context, doc account.Document) (account.Account, error)
	Balance(ctx context.Context, id account.ID) (account.Money, error)
	Statement(ctx context.Context, id account.ID, cursor int64, pageSize int) (account.StatementPage, error)
}

// LedgerHandler implementa conta.ledger.v1.LedgerService: converte o pedido em comando
// validado, chama o caso de uso e converte o resultado. Os erros saem crus; o
// errorInterceptor os traduz para status gRPC.
type LedgerHandler struct {
	ledgerv1.UnimplementedLedgerServiceServer
	svc LedgerService
}

// NewLedgerHandler cria o handler sobre os casos de uso.
func NewLedgerHandler(svc LedgerService) *LedgerHandler {
	return &LedgerHandler{svc: svc}
}

// RegisterLedger registra o handler no servidor.
func RegisterLedger(srv grpc.ServiceRegistrar, h ledgerv1.LedgerServiceServer) {
	ledgerv1.RegisterLedgerServiceServer(srv, h)
}

// OpenAccount abre uma conta.
func (h *LedgerHandler) OpenAccount(ctx context.Context, req *ledgerv1.OpenAccountRequest) (*ledgerv1.OpenAccountResponse, error) {
	key, err := account.NewIdempotencyKey(req.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	holder, err := account.NewHolderName(req.GetHolderName())
	if err != nil {
		return nil, err
	}
	doc, err := account.ParseCPF(req.GetHolderDocument())
	if err != nil {
		return nil, err
	}
	a, err := h.svc.OpenAccount(ctx, account.OpenAccountCommand{Key: key, Holder: holder, Document: doc})
	if err != nil {
		return nil, err
	}
	return &ledgerv1.OpenAccountResponse{Account: accountToProto(a)}, nil
}

// GetAccount consulta os dados cadastrais.
func (h *LedgerHandler) GetAccount(ctx context.Context, req *ledgerv1.GetAccountRequest) (*ledgerv1.GetAccountResponse, error) {
	id, err := account.ParseID(req.GetAccountId())
	if err != nil {
		return nil, err
	}
	a, err := h.svc.Account(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ledgerv1.GetAccountResponse{Account: accountToProto(a)}, nil
}

// GetAccountByDocument busca a conta pelo CPF do titular.
func (h *LedgerHandler) GetAccountByDocument(ctx context.Context, req *ledgerv1.GetAccountByDocumentRequest) (*ledgerv1.GetAccountByDocumentResponse, error) {
	doc, err := account.ParseCPF(req.GetHolderDocument())
	if err != nil {
		return nil, err
	}
	a, err := h.svc.AccountByDocument(ctx, doc)
	if err != nil {
		return nil, err
	}
	return &ledgerv1.GetAccountByDocumentResponse{Account: accountToProto(a)}, nil
}

// movementInput são os campos comuns de Credit e Debit, já validados.
type movementInput struct {
	key    account.IdempotencyKey
	id     account.ID
	amount account.Money
	desc   account.Description
}

func parseMovement(key, accountID string, amountCents int64, description string) (movementInput, error) {
	k, err := account.NewIdempotencyKey(key)
	if err != nil {
		return movementInput{}, err
	}
	id, err := account.ParseID(accountID)
	if err != nil {
		return movementInput{}, err
	}
	amount, err := account.NewAmount(amountCents)
	if err != nil {
		return movementInput{}, err
	}
	desc, err := account.NewDescription(description)
	if err != nil {
		return movementInput{}, err
	}
	return movementInput{key: k, id: id, amount: amount, desc: desc}, nil
}

// Credit credita uma conta.
func (h *LedgerHandler) Credit(ctx context.Context, req *ledgerv1.CreditRequest) (*ledgerv1.CreditResponse, error) {
	in, err := parseMovement(req.GetIdempotencyKey(), req.GetAccountId(), req.GetAmountCents(), req.GetDescription())
	if err != nil {
		return nil, err
	}
	reason, err := creditReasonFromProto(req.GetReason())
	if err != nil {
		return nil, err
	}
	p, err := h.svc.Credit(ctx, account.CreditCommand{
		Key: in.key, AccountID: in.id, Amount: in.amount, Reason: reason, Description: in.desc,
	})
	if err != nil {
		return nil, err
	}
	return &ledgerv1.CreditResponse{Posting: postingToProto(p)}, nil
}

// Debit debita uma conta.
func (h *LedgerHandler) Debit(ctx context.Context, req *ledgerv1.DebitRequest) (*ledgerv1.DebitResponse, error) {
	in, err := parseMovement(req.GetIdempotencyKey(), req.GetAccountId(), req.GetAmountCents(), req.GetDescription())
	if err != nil {
		return nil, err
	}
	reason, err := debitReasonFromProto(req.GetReason())
	if err != nil {
		return nil, err
	}
	p, err := h.svc.Debit(ctx, account.DebitCommand{
		Key: in.key, AccountID: in.id, Amount: in.amount, Reason: reason, Description: in.desc,
	})
	if err != nil {
		return nil, err
	}
	return &ledgerv1.DebitResponse{Posting: postingToProto(p)}, nil
}

// GetBalance devolve o saldo atual.
func (h *LedgerHandler) GetBalance(ctx context.Context, req *ledgerv1.GetBalanceRequest) (*ledgerv1.GetBalanceResponse, error) {
	id, err := account.ParseID(req.GetAccountId())
	if err != nil {
		return nil, err
	}
	b, err := h.svc.Balance(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ledgerv1.GetBalanceResponse{AccountId: id.String(), BalanceCents: b.Cents()}, nil
}

// ListStatement devolve uma página do extrato.
func (h *LedgerHandler) ListStatement(ctx context.Context, req *ledgerv1.ListStatementRequest) (*ledgerv1.ListStatementResponse, error) {
	id, err := account.ParseID(req.GetAccountId())
	if err != nil {
		return nil, err
	}
	cursor, err := decodePageToken(req.GetPageToken())
	if err != nil {
		return nil, err
	}
	page, err := h.svc.Statement(ctx, id, cursor, int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	resp := &ledgerv1.ListStatementResponse{
		Postings:      make([]*ledgerv1.Posting, 0, len(page.Lines)),
		NextPageToken: encodePageToken(page.Next),
	}
	for _, l := range page.Lines {
		resp.Postings = append(resp.Postings, postingToProto(l.Posting))
	}
	return resp, nil
}
