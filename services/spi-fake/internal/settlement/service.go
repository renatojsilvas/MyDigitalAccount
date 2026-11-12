package settlement

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Store guarda as liquidações. Implementado sobre o Postgres (internal/postgres).
type Store interface {
	// Insert grava a liquidação se o ID for novo; inserted = false se já existia.
	Insert(ctx context.Context, s Settlement) (inserted bool, err error)
	// Get lê uma liquidação. ErrNotFound se não existir.
	Get(ctx context.Context, id uuid.UUID) (Settlement, error)
}

// QueryResult é o desfecho de uma consulta de status; vira rótulo de métrica (conjunto fixo).
type QueryResult string

// Desfechos da consulta.
const (
	QueryFound    QueryResult = "found"
	QueryNotFound QueryResult = "not_found"
	QueryFailed   QueryResult = "failed"
)

// SettleEvent descreve um Settle respondido, ou segurado sem resposta, para observabilidade.
type SettleEvent struct {
	Mode Mode
	// Settlement é o registrado; quando o modo não liquidou (NoSettleNoResponse), só o pedido
	// vem preenchido e Status é zero.
	Settlement Settlement
	// Replay: o ID já existia e o resultado registrado foi devolvido.
	Replay bool
	// Held: a resposta foi segurada (modos "sem responder").
	Held bool
}

// Observer recebe os eventos, para métricas e atributos de trace.
type Observer interface {
	Settled(ctx context.Context, ev SettleEvent)
	StatusQueried(ctx context.Context, id uuid.UUID, result QueryResult)
}

type noopObserver struct{}

func (noopObserver) Settled(context.Context, SettleEvent)                  {}
func (noopObserver) StatusQueried(context.Context, uuid.UUID, QueryResult) {}

// Service são os casos de uso do simulador.
type Service struct {
	store    Store
	behavior *Switch
	maxHold  time.Duration
	now      func() time.Time
	observer Observer
}

// Option configura o Service (functional options).
type Option func(*Service)

// WithClock troca o relógio, para testes determinísticos.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithObserver liga o Service a um Observer.
func WithObserver(o Observer) Option { return func(s *Service) { s.observer = o } }

// NewService monta os casos de uso. maxHold limita quanto tempo os modos "sem responder"
// seguram uma chamada cujo cliente não tem prazo, para nenhuma goroutine ficar presa.
func NewService(store Store, behavior *Switch, maxHold time.Duration, opts ...Option) *Service {
	s := &Service{store: store, behavior: behavior, maxHold: maxHold, now: time.Now, observer: noopObserver{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Settle liquida (ou recusa) o pedido conforme o comportamento em vigor. É idempotente pelo
// ID: um ID já registrado devolve o resultado registrado, que nunca muda, qualquer que seja o
// modo atual. O mesmo ID com outro pedido é ErrIDReused.
func (s *Service) Settle(ctx context.Context, req Request) (Settlement, error) {
	b := s.behavior.Current()
	if err := wait(ctx, b.Latency); err != nil {
		return Settlement{}, err
	}

	if b.Mode == ModeNoSettleNoResponse {
		s.observer.Settled(ctx, SettleEvent{Mode: b.Mode, Settlement: Settlement{Request: req}, Held: true})
		return Settlement{}, s.hold(ctx)
	}

	decided := Settlement{Request: req, Status: Settled, DecidedAt: s.now().UTC().Truncate(time.Microsecond)}
	if b.Mode == ModeReject {
		decided.Status, decided.RejectReason = Rejected, RejectSimulated
	}
	inserted, err := s.store.Insert(ctx, decided)
	if err != nil {
		return Settlement{}, err
	}
	result := decided
	if !inserted {
		if result, err = s.store.Get(ctx, req.ID); err != nil {
			return Settlement{}, err
		}
		if !result.Matches(req) {
			return Settlement{}, ErrIDReused
		}
	}

	ev := SettleEvent{Mode: b.Mode, Settlement: result, Replay: !inserted}
	if b.Mode == ModeSettleNoResponse {
		ev.Held = true
		s.observer.Settled(ctx, ev)
		return Settlement{}, s.hold(ctx)
	}
	s.observer.Settled(ctx, ev)
	return result, nil
}

// Get consulta uma liquidação pelo ID.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Settlement, error) {
	if s.behavior.Current().FailStatusQueries {
		s.observer.StatusQueried(ctx, id, QueryFailed)
		return Settlement{}, ErrStatusUnavailable
	}
	st, err := s.store.Get(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		s.observer.StatusQueried(ctx, id, QueryNotFound)
	case err != nil:
		s.observer.StatusQueried(ctx, id, QueryFailed)
	default:
		s.observer.StatusQueried(ctx, id, QueryFound)
	}
	return st, err
}

// hold segura a resposta até o cliente desistir (prazo ou cancelamento) ou até maxHold. O
// cliente vê timeout: é o "liquidou, mas não respondeu" (ou "não liquidou nem respondeu").
func (s *Service) hold(ctx context.Context) error {
	t := time.NewTimer(s.maxHold)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return ErrNoResponse
	}
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
