package settlement_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"conta.local/spi-fake/internal/settlement"
)

// memStore é o Store em memória; a versão real (Postgres) tem os próprios testes.
type memStore struct {
	mu sync.Mutex
	m  map[uuid.UUID]settlement.Settlement
}

func newMemStore() *memStore { return &memStore{m: map[uuid.UUID]settlement.Settlement{}} }

func (s *memStore) Insert(_ context.Context, st settlement.Settlement) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[st.ID]; ok {
		return false, nil
	}
	s.m[st.ID] = st
	return true, nil
}

func (s *memStore) Get(_ context.Context, id uuid.UUID) (settlement.Settlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[id]
	if !ok {
		return settlement.Settlement{}, settlement.ErrNotFound
	}
	return st, nil
}

type recorder struct {
	mu      sync.Mutex
	events  []settlement.SettleEvent
	queries []settlement.QueryResult
}

func (r *recorder) Settled(_ context.Context, ev settlement.SettleEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) StatusQueried(_ context.Context, _ uuid.UUID, q settlement.QueryResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, q)
}

func newRequest(t *testing.T) settlement.Request {
	t.Helper()
	r, err := settlement.NewRequest(uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), 1_000)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type fixture struct {
	svc   *settlement.Service
	sw    *settlement.Switch
	store *memStore
	rec   *recorder
}

func newFixture(mode settlement.Mode) fixture {
	f := fixture{sw: settlement.NewSwitch(settlement.Behavior{Mode: mode}), store: newMemStore(), rec: &recorder{}}
	f.svc = settlement.NewService(f.store, f.sw, 5*time.Second, settlement.WithObserver(f.rec))
	return f
}

// shortCtx simula o prazo do cliente (o transfers espera 1 s por tentativa; aqui, 30 ms).
func shortCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestSettleModes(t *testing.T) {
	tests := []struct {
		name         string
		mode         settlement.Mode
		wantErr      error
		wantStatus   settlement.Status // o que GetSettlement mostra depois
		wantNotFound bool
	}{
		{"liquida e responde", settlement.ModeSettle, nil, settlement.Settled, false},
		{"recusa e responde", settlement.ModeReject, nil, settlement.Rejected, false},
		{"liquida sem responder", settlement.ModeSettleNoResponse, context.DeadlineExceeded, settlement.Settled, false},
		{"não liquida nem responde", settlement.ModeNoSettleNoResponse, context.DeadlineExceeded, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(tt.mode)
			req := newRequest(t)
			got, err := f.svc.Settle(shortCtx(t), req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && got.Status != tt.wantStatus {
				t.Fatalf("resposta = %v, want %v", got.Status, tt.wantStatus)
			}
			st, err := f.svc.Get(t.Context(), req.ID)
			if tt.wantNotFound {
				if !errors.Is(err, settlement.ErrNotFound) {
					t.Fatalf("consulta = %+v, %v; want ErrNotFound", st, err)
				}
				return
			}
			if err != nil || st.Status != tt.wantStatus || !st.Matches(req) {
				t.Fatalf("consulta = %+v, %v; want %v", st, err, tt.wantStatus)
			}
			if tt.mode == settlement.ModeReject && st.RejectReason != settlement.RejectSimulated {
				t.Fatalf("motivo da recusa = %v", st.RejectReason)
			}
		})
	}
}

// Mesmo ID, mesmo pedido: devolve o registrado, que não muda nem quando o modo muda.
func TestSettleIsIdempotentAndResultNeverChanges(t *testing.T) {
	f := newFixture(settlement.ModeSettleNoResponse)
	req := newRequest(t)
	if _, err := f.svc.Settle(shortCtx(t), req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("primeira tentativa: %v, want timeout", err)
	}

	// O transfers tenta de novo com o mesmo ID; agora o SPI responde, mas em modo recusa: o
	// resultado continua o registrado na primeira vez (liquidado).
	if err := f.sw.Set(settlement.Behavior{Mode: settlement.ModeReject}); err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.Settle(t.Context(), req)
	if err != nil || again.Status != settlement.Settled {
		t.Fatalf("repetição = %+v, %v; want o liquidado registrado", again, err)
	}
	if last := f.rec.events[len(f.rec.events)-1]; !last.Replay {
		t.Fatalf("evento da repetição = %+v, want Replay", last)
	}

	other := req
	other.AmountCents = 999
	if _, err := f.svc.Settle(t.Context(), other); !errors.Is(err, settlement.ErrIDReused) {
		t.Fatalf("mesmo ID com outro valor: %v, want ErrIDReused", err)
	}
}

func TestFailStatusQueries(t *testing.T) {
	f := newFixture(settlement.ModeSettle)
	req := newRequest(t)
	if _, err := f.svc.Settle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := f.sw.Set(settlement.Behavior{Mode: settlement.ModeSettle, FailStatusQueries: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Get(t.Context(), req.ID); !errors.Is(err, settlement.ErrStatusUnavailable) {
		t.Fatalf("err = %v, want ErrStatusUnavailable", err)
	}
	// O Settle continua funcionando: a falha é só da consulta.
	if _, err := f.svc.Settle(t.Context(), newRequest(t)); err != nil {
		t.Fatalf("Settle com consultas falhando: %v", err)
	}
	want := []settlement.QueryResult{settlement.QueryFailed}
	if len(f.rec.queries) != 1 || f.rec.queries[0] != want[0] {
		t.Fatalf("consultas observadas = %v, want %v", f.rec.queries, want)
	}
}

// Sem prazo do cliente, o "sem responder" para no teto, sem prender a goroutine para sempre.
func TestHoldIsBounded(t *testing.T) {
	sw := settlement.NewSwitch(settlement.Behavior{Mode: settlement.ModeNoSettleNoResponse})
	svc := settlement.NewService(newMemStore(), sw, 20*time.Millisecond)
	if _, err := svc.Settle(context.Background(), newRequest(t)); !errors.Is(err, settlement.ErrNoResponse) {
		t.Fatalf("err = %v, want ErrNoResponse", err)
	}
}

func TestLatency(t *testing.T) {
	f := newFixture(settlement.ModeSettle)
	if err := f.sw.Set(settlement.Behavior{Mode: settlement.ModeSettle, Latency: 40 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := f.svc.Settle(t.Context(), newRequest(t)); err != nil || time.Since(start) < 40*time.Millisecond {
		t.Fatalf("Settle = %v em %v; want sucesso depois de 40 ms", err, time.Since(start))
	}
}

func TestBehaviorValidationAndParse(t *testing.T) {
	sw := settlement.NewSwitch(settlement.Behavior{Mode: settlement.ModeSettle})
	for _, bad := range []settlement.Behavior{{}, {Mode: 99}, {Mode: settlement.ModeSettle, Latency: -1}} {
		if err := sw.Set(bad); err == nil {
			t.Errorf("aceitou %+v", bad)
		}
	}
	if sw.Current().Mode != settlement.ModeSettle {
		t.Fatal("comportamento inválido foi aplicado")
	}
	for _, m := range settlement.Modes() {
		if got, err := settlement.ParseMode(m.String()); err != nil || got != m {
			t.Errorf("ParseMode(%q) = %v, %v", m.String(), got, err)
		}
	}
	if _, err := settlement.ParseMode("liquidar"); err == nil {
		t.Error("modo desconhecido aceito")
	}
}

func TestNewRequestCarriesEveryField(t *testing.T) {
	ids := [4]uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	r, err := settlement.NewRequest(ids[0].String(), ids[1].String(), ids[2].String(), ids[3].String(), 12_345)
	if err != nil {
		t.Fatal(err)
	}
	want := settlement.Request{ID: ids[0], TransferID: ids[1], Payer: ids[2], Payee: ids[3], AmountCents: 12_345}
	if r != want {
		t.Fatalf("pedido = %+v, want %+v", r, want)
	}
}

func TestNewRequestValidation(t *testing.T) {
	id := uuid.NewString()
	tests := []struct {
		name                        string
		sid, transfer, payer, payee string
		amount                      int64
		field                       string
	}{
		{"settlement_id", "x", id, id, id, 1, "settlement_id"},
		{"transfer_id", id, "", id, id, 1, "transfer_id"},
		{"payer", id, id, "x", id, 1, "payer_account_id"},
		{"payee", id, id, id, "x", 1, "payee_account_id"},
		{"valor", id, id, id, id, 0, "amount_cents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := settlement.NewRequest(tt.sid, tt.transfer, tt.payer, tt.payee, tt.amount)
			var verr *settlement.ValidationError
			if !errors.As(err, &verr) || verr.Field != tt.field {
				t.Fatalf("err = %v, want campo %s", err, tt.field)
			}
		})
	}
}
