package clients_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	antifraudev1 "conta.local/gen/go/conta/antifraude/v1"
	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/transfers/internal/clients"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

type obs struct {
	mu      sync.Mutex
	retries map[string]int
	states  []resilience.State
}

func newObs() *obs { return &obs{retries: map[string]int{}} }

func (o *obs) Retried(dep string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.retries[dep]++
}

func (o *obs) BreakerState(_ string, s resilience.State) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, s)
}

func dial(t *testing.T, register func(*grpc.Server)) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	register(srv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func withReason(code codes.Code, reason string) error {
	st, _ := status.New(code, "recusa").WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "ledger.conta.local"})
	return st.Err()
}

var fastPolicy = resilience.Policy{Attempts: 3, Base: time.Millisecond, Max: 2 * time.Millisecond}

func dependency(name string, o *obs) *clients.Dependency {
	return clients.NewDependency(name, resilience.BreakerConfig{
		ConsecutiveFailures: 3, FailureRatio: 1, MinRequests: 100, Window: time.Minute, OpenFor: time.Minute,
	}, fastPolicy, o)
}

// --- ledger ---

type fakeLedger struct {
	ledgerv1.UnimplementedLedgerServiceServer
	err   error
	calls atomic.Int32
}

func (f *fakeLedger) GetAccountByDocument(context.Context, *ledgerv1.GetAccountByDocumentRequest) (*ledgerv1.GetAccountByDocumentResponse, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &ledgerv1.GetAccountByDocumentResponse{Account: &ledgerv1.Account{AccountId: uuid.NewString()}}, nil
}

func (f *fakeLedger) Debit(context.Context, *ledgerv1.DebitRequest) (*ledgerv1.DebitResponse, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &ledgerv1.DebitResponse{Posting: &ledgerv1.Posting{EntryId: uuid.NewString()}}, nil
}

func (f *fakeLedger) Reverse(context.Context, *ledgerv1.ReverseRequest) (*ledgerv1.ReverseResponse, error) {
	f.calls.Add(1)
	return nil, f.err
}

func TestLedgerErrorTranslation(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		call      func(l *clients.Ledger) error
		want      error
		wantCalls int32
	}{
		{"conta do destino não existe", withReason(codes.NotFound, "ERROR_REASON_ACCOUNT_NOT_FOUND"),
			func(l *clients.Ledger) error { _, err := l.AccountByDocument(context.Background(), "x"); return err },
			saga.ErrAccountNotFound, 1},
		{"saldo insuficiente não repete", withReason(codes.FailedPrecondition, "ERROR_REASON_INSUFFICIENT_FUNDS"),
			func(l *clients.Ledger) error {
				_, err := l.Debit(context.Background(), "k", uuid.New(), 1, "r", "d")
				return err
			}, saga.ErrInsufficientFunds, 1},
		{"conta de origem não existe", withReason(codes.NotFound, "ERROR_REASON_ACCOUNT_NOT_FOUND"),
			func(l *clients.Ledger) error {
				_, err := l.Debit(context.Background(), "k", uuid.New(), 1, "r", "d")
				return err
			}, saga.ErrAccountNotFound, 1},
		{"fora do ar: repete e desiste", status.Error(codes.Unavailable, "fora"),
			func(l *clients.Ledger) error {
				_, err := l.Debit(context.Background(), "k", uuid.New(), 1, "r", "d")
				return err
			}, saga.ErrUnavailable, 3},
		{"estorno já feito conta como sucesso", withReason(codes.FailedPrecondition, "ERROR_REASON_ENTRY_ALREADY_REVERSED"),
			func(l *clients.Ledger) error { return l.Reverse(context.Background(), "k", uuid.New(), "r", "d") }, nil, 1},
		{"estorno não estornável é falha", withReason(codes.FailedPrecondition, "ERROR_REASON_ENTRY_NOT_REVERSIBLE"),
			func(l *clients.Ledger) error { return l.Reverse(context.Background(), "k", uuid.New(), "r", "d") },
			saga.ErrUnavailable, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeLedger{err: tt.err}
			conn := dial(t, func(s *grpc.Server) { ledgerv1.RegisterLedgerServiceServer(s, fake) })
			l := clients.NewLedger(ledgerv1.NewLedgerServiceClient(conn), dependency("ledger", newObs()))
			if err := tt.call(l); !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if got := fake.calls.Load(); got != tt.wantCalls {
				t.Fatalf("%d chamadas, want %d", got, tt.wantCalls)
			}
		})
	}
}

// --- antifraude ---

type fakeAntifraude struct {
	antifraudev1.UnimplementedAntifraudeServiceServer
	failFirst int32
	calls     atomic.Int32
}

func (f *fakeAntifraude) Evaluate(context.Context, *antifraudev1.EvaluateRequest) (*antifraudev1.EvaluateResponse, error) {
	if f.calls.Add(1) <= f.failFirst {
		return nil, status.Error(codes.Unavailable, "falha injetada")
	}
	return &antifraudev1.EvaluateResponse{Decision: antifraudev1.Decision_DECISION_DENY}, nil
}

func TestAntifraudeRetriesTransientFailures(t *testing.T) {
	fake := &fakeAntifraude{failFirst: 2}
	conn := dial(t, func(s *grpc.Server) { antifraudev1.RegisterAntifraudeServiceServer(s, fake) })
	o := newObs()
	a := clients.NewAntifraude(antifraudev1.NewAntifraudeServiceClient(conn), dependency("antifraude", o))
	approved, err := a.Evaluate(t.Context(), uuid.New(), uuid.New(), 1)
	if err != nil || approved || fake.calls.Load() != 3 || o.retries["antifraude"] != 2 {
		t.Fatalf("aprovado=%v err=%v chamadas=%d retries=%d; want recusa na 3ª, 2 retries",
			approved, err, fake.calls.Load(), o.retries["antifraude"])
	}
}

// --- SPI ---

type fakeSPI struct {
	spiv1.UnimplementedSpiServiceServer
	err      error
	status   spiv1.SettlementStatus
	received atomic.Int32
}

func (f *fakeSPI) Settle(context.Context, *spiv1.SettleRequest) (*spiv1.SettleResponse, error) {
	f.received.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &spiv1.SettleResponse{Settlement: &spiv1.Settlement{Status: f.status}}, nil
}

func settleRequest() saga.SettleRequest {
	return saga.SettleRequest{SettlementID: uuid.New(), TransferID: uuid.New(), Payer: uuid.New(), Payee: uuid.New(), AmountCents: 1}
}

// O marcador é gravado antes de a chamada sair; com o circuito aberto, nem marcador nem chamada.
func TestSPIMarksBeforeSendingAndNeverSendsWhenOpen(t *testing.T) {
	fake := &fakeSPI{err: status.Error(codes.DeadlineExceeded, "sem resposta")}
	conn := dial(t, func(s *grpc.Server) { spiv1.RegisterSpiServiceServer(s, fake) })
	o := newObs()
	spi := clients.NewSPI(spiv1.NewSpiServiceClient(conn), dependency("spi", o))

	var marks int
	mark := func() error {
		if fake.received.Load() != int32(marks) { //nolint:gosec // contadores pequenos
			t.Error("a chamada saiu antes do marcador")
		}
		marks++
		return nil
	}
	for range 3 { // três timeouts seguidos abrem o circuito
		if _, err := spi.Settle(t.Context(), settleRequest(), mark); status.Code(err) != codes.DeadlineExceeded {
			t.Fatalf("err = %v, want o timeout (ambíguo)", err)
		}
	}
	_, err := spi.Settle(t.Context(), settleRequest(), mark)
	if !errors.Is(err, resilience.ErrNotSent) || marks != 3 || fake.received.Load() != 3 {
		t.Fatalf("circuito aberto: err=%v marcadores=%d recebidas=%d; want ErrNotSent sem marcar nem enviar",
			err, marks, fake.received.Load())
	}
	if len(o.states) != 1 || o.states[0] != resilience.Open {
		t.Fatalf("estados do circuito = %v, want [open]", o.states)
	}
}

func TestSPIResponses(t *testing.T) {
	tests := []struct {
		name   string
		status spiv1.SettlementStatus
		want   saga.SettleStatus
	}{
		{"liquidado", spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED, saga.Settled},
		{"recusado", spiv1.SettlementStatus_SETTLEMENT_STATUS_REJECTED, saga.Rejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeSPI{status: tt.status}
			conn := dial(t, func(s *grpc.Server) { spiv1.RegisterSpiServiceServer(s, fake) })
			spi := clients.NewSPI(spiv1.NewSpiServiceClient(conn), dependency("spi", newObs()))
			got, err := spi.Settle(t.Context(), settleRequest(), func() error { return nil })
			if err != nil || got != tt.want {
				t.Fatalf("= %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// Marcador não gravado: nada sai, e o erro é "não enviado".
func TestSPIDoesNotSendWhenMarkFails(t *testing.T) {
	fake := &fakeSPI{status: spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED}
	conn := dial(t, func(s *grpc.Server) { spiv1.RegisterSpiServiceServer(s, fake) })
	spi := clients.NewSPI(spiv1.NewSpiServiceClient(conn), dependency("spi", newObs()))
	_, err := spi.Settle(t.Context(), settleRequest(), func() error { return errors.New("banco fora") })
	if !errors.Is(err, resilience.ErrNotSent) || fake.received.Load() != 0 {
		t.Fatalf("err=%v recebidas=%d; want ErrNotSent sem chamada", err, fake.received.Load())
	}
}
