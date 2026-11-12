package saga_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

type fakeAntifraud struct {
	approve bool
	err     error
	calls   int
}

func (f *fakeAntifraud) Evaluate(context.Context, uuid.UUID, uuid.UUID, int64) (bool, error) {
	f.calls++
	return f.approve, f.err
}

// fakeLedger imita o ledger: contas por CPF, saldos, e escritas idempotentes pela chave (a
// mesma chave devolve o mesmo resultado, sem efeito novo).
type fakeLedger struct {
	mu        sync.Mutex
	byCPF     map[string]uuid.UUID
	balance   map[uuid.UUID]int64
	results   map[string]uuid.UUID // chave -> lançamento
	debits    int
	credits   int
	reversals int
	// failDebit: quantas vezes o débito "some" (não responde) antes de responder. Com
	// applyThenFail, o débito é aplicado e só a resposta se perde (queda no meio).
	failDebit     int
	applyThenFail bool
	lookupErr     error
}

func newLedger() *fakeLedger {
	return &fakeLedger{byCPF: map[string]uuid.UUID{}, balance: map[uuid.UUID]int64{}, results: map[string]uuid.UUID{}}
}

func (l *fakeLedger) open(cpf string, cents int64) uuid.UUID {
	id := uuid.New()
	l.byCPF[cpf] = id
	l.balance[id] = cents
	return id
}

func (l *fakeLedger) AccountByDocument(_ context.Context, cpf string) (uuid.UUID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lookupErr != nil {
		return uuid.UUID{}, l.lookupErr
	}
	id, ok := l.byCPF[cpf]
	if !ok {
		return uuid.UUID{}, saga.ErrAccountNotFound
	}
	return id, nil
}

func (l *fakeLedger) Debit(_ context.Context, key string, acc uuid.UUID, cents int64, _, _ string) (uuid.UUID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failDebit > 0 && !l.applyThenFail {
		l.failDebit--
		return uuid.UUID{}, saga.ErrUnavailable
	}
	if e, ok := l.results[key]; ok {
		return e, nil
	}
	bal, ok := l.balance[acc]
	if !ok {
		return uuid.UUID{}, saga.ErrAccountNotFound
	}
	if bal < cents {
		return uuid.UUID{}, saga.ErrInsufficientFunds
	}
	l.balance[acc] = bal - cents
	l.debits++
	e := uuid.New()
	l.results[key] = e
	if l.failDebit > 0 {
		l.failDebit--
		return uuid.UUID{}, saga.ErrUnavailable // aplicado, resposta perdida
	}
	return e, nil
}

func (l *fakeLedger) Credit(_ context.Context, key string, acc uuid.UUID, cents int64, _, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.results[key]; ok {
		return nil
	}
	l.balance[acc] += cents
	l.credits++
	l.results[key] = uuid.New()
	return nil
}

func (l *fakeLedger) Reverse(_ context.Context, key string, entry uuid.UUID, _, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.results[key]; ok {
		return nil
	}
	// Devolve à conta de origem o valor do débito original.
	for k, e := range l.results {
		if e == entry && len(k) > 6 && k[len(k)-6:] == ":debit" {
			l.reversals++
			l.results[key] = uuid.New()
			return nil
		}
	}
	return errors.New("lançamento não encontrado")
}

// attempt é o que o SPI falso faz numa tentativa.
type attempt int

const (
	spiSettled  attempt = iota // liquida e responde
	spiRejected                // recusa e responde
	spiTimeout                 // a chamada sai, sem resposta (ambíguo)
	spiNotSent                 // o circuito recusa: nada sai
)

type fakeSPI struct {
	mu     sync.Mutex
	script []attempt
	calls  int
	sent   int // chamadas que de fato saíram (depois do beforeSend)
}

func (s *fakeSPI) Settle(_ context.Context, _ saga.SettleRequest, beforeSend func() error) (saga.SettleStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.script[min(s.calls, len(s.script)-1)]
	s.calls++
	if a == spiNotSent {
		return 0, resilience.ErrNotSent
	}
	if err := beforeSend(); err != nil {
		return 0, err
	}
	s.sent++
	switch a {
	case spiSettled:
		return saga.Settled, nil
	case spiRejected:
		return saga.Rejected, nil
	default:
		return 0, status.Error(codes.DeadlineExceeded, "sem resposta")
	}
}

type noTracing struct{}

func (noTracing) Start(ctx context.Context, _ map[string]string, _, _ string) (context.Context, func(error)) {
	return ctx, func(error) {}
}

type recorder struct {
	mu            sync.Mutex
	transitions   []string
	compensations map[string]int
	settle        map[string]int
	leaseLost     int
}

func newRecorder() *recorder {
	return &recorder{compensations: map[string]int{}, settle: map[string]int{}}
}

func (r *recorder) Transitioned(_ context.Context, from, to pix.State, _ pix.Reason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitions = append(r.transitions, from.String()+"->"+to.String())
}
func (r *recorder) StepFinished(context.Context, pix.State, string, time.Duration) {}
func (r *recorder) Compensated(_ context.Context, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.compensations[kind]++
}
func (r *recorder) SettleAttempt(_ context.Context, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settle[result]++
}
func (r *recorder) LeaseLost(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaseLost++
}
