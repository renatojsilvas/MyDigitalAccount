package pix

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// AcceptRecord é o que o aceite grava, numa só transação: a transferência, a primeira linha
// do histórico e a chave de idempotência (só com o HMAC do pedido, sem dado pessoal).
type AcceptRecord struct {
	Transfer    Transfer
	Key         IdempotencyKey
	RequestHMAC []byte
	NextRunAt   time.Time
}

// Lease é a posse de uma transferência por um runner (ADR-0007, seção 4). Token é o fencing
// token: toda escrita confere que ele ainda é o atual.
type Lease struct {
	TransferID uuid.UUID
	Token      int64
}

// Store é a persistência do transfers, definida aqui, por quem a consome.
type Store interface {
	// Accept grava o aceite. Se a chave já existe para a conta: com o mesmo HMAC, devolve a
	// transferência existente (created = false); com outro, ErrIdempotencyKeyReused.
	Accept(ctx context.Context, rec AcceptRecord) (t Transfer, created bool, err error)
	// Get devolve a transferência da conta e o histórico. ErrNotFound se não existir ou se
	// for de outra conta.
	Get(ctx context.Context, accountID, id uuid.UUID) (Transfer, []Change, error)
	// UsedOn devolve quanto do limite da conta está reservado no dia.
	UsedOn(ctx context.Context, accountID uuid.UUID, day Day) (int64, error)
}

// Observer recebe os eventos do aceite, para métricas e atributos de trace.
type Observer interface {
	Started(ctx context.Context, t Transfer, created bool)
	KeyConflict(ctx context.Context)
}

type noopObserver struct{}

func (noopObserver) Started(context.Context, Transfer, bool) {}
func (noopObserver) KeyConflict(context.Context)             {}

// Service são os casos de uso de leitura e aceite. A saga (runner) é outro componente.
type Service struct {
	store      Store
	cal        Calendar
	hmacKey    []byte
	dailyLimit int64
	now        func() time.Time
	observer   Observer
	notify     func(uuid.UUID)
}

// Option configura o Service (functional options).
type Option func(*Service)

// WithClock troca o relógio, para testes determinísticos.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithObserver liga o Service a um Observer.
func WithObserver(o Observer) Option { return func(s *Service) { s.observer = o } }

// WithNotify registra quem é avisado de um Pix novo (o runner da saga, caminho rápido). O
// banco continua sendo a fonte da verdade: um aviso perdido é coberto pela varredura.
func WithNotify(fn func(uuid.UUID)) Option { return func(s *Service) { s.notify = fn } }

// NewService monta os casos de uso. hmacKey assina o pedido na tabela de idempotência.
func NewService(store Store, cal Calendar, hmacKey []byte, dailyLimitCents int64, opts ...Option) *Service {
	s := &Service{
		store: store, cal: cal, hmacKey: hmacKey, dailyLimit: dailyLimitCents,
		now: time.Now, observer: noopObserver{}, notify: func(uuid.UUID) {},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start aceita o Pix: grava a transferência em ACCEPTED, com o settlement_id já gerado, e
// devolve. Não espera a saga (ADR-0007, seção 4). Repetir com a mesma chave e o mesmo pedido
// devolve o mesmo Pix, no estado em que ele estiver.
func (s *Service) Start(ctx context.Context, cmd StartCommand) (Transfer, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return Transfer{}, fmt.Errorf("pix: gerando id: %w", err)
	}
	settlementID, err := uuid.NewV7()
	if err != nil {
		return Transfer{}, fmt.Errorf("pix: gerando settlement_id: %w", err)
	}
	now := s.instant()
	t := Transfer{
		ID: id, SourceAccountID: cmd.AccountID, DestinationKey: cmd.DestinationKey,
		AmountCents: cmd.AmountCents, Description: cmd.Description, State: StateAccepted,
		SettlementID: settlementID, LimitDay: s.cal.DayOf(now), Trace: cmd.Trace,
		CreatedAt: now, UpdatedAt: now,
	}
	got, created, err := s.store.Accept(ctx, AcceptRecord{
		Transfer: t, Key: cmd.Key, RequestHMAC: s.requestHMAC(cmd), NextRunAt: now,
	})
	if err != nil {
		if errors.Is(err, ErrIdempotencyKeyReused) {
			s.observer.KeyConflict(ctx)
		}
		return Transfer{}, err
	}
	s.observer.Started(ctx, got, created)
	if created {
		s.notify(got.ID)
	}
	return got, nil
}

// Get devolve o Pix da conta, com o histórico.
func (s *Service) Get(ctx context.Context, accountID, id uuid.UUID) (Transfer, []Change, error) {
	return s.store.Get(ctx, accountID, id)
}

// Limits devolve o limite diário da conta no dia corrente.
func (s *Service) Limits(ctx context.Context, accountID uuid.UUID) (Limits, error) {
	day := s.cal.DayOf(s.now())
	used, err := s.store.UsedOn(ctx, accountID, day)
	if err != nil {
		return Limits{}, err
	}
	return NewLimits(s.dailyLimit, used, s.cal.ResetsAt(day)), nil
}

// instant devolve o instante atual em UTC com a precisão do Postgres (microssegundos), para o
// valor devolvido no aceite ser igual ao lido do banco depois.
func (s *Service) instant() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// requestHMAC resume o pedido com HMAC-SHA256. Um SHA-256 simples de um pedido com CPF e valor
// poderia ser revertido por força bruta; com a chave secreta, a tabela de idempotência não
// guarda nada que leve ao dado pessoal (ADR-0007, seção 4). Cada campo vai com o tamanho na
// frente, para ("ab","c") e ("a","bc") não colidirem.
func (s *Service) requestHMAC(cmd StartCommand) []byte {
	mac := hmac.New(sha256.New, s.hmacKey)
	var size [4]byte
	for _, f := range []string{
		cmd.AccountID.String(), cmd.DestinationKey.Type().String(), cmd.DestinationKey.Value(),
		strconv.FormatInt(cmd.AmountCents, 10), cmd.Description,
	} {
		binary.BigEndian.PutUint32(size[:], uint32(len(f))) //nolint:gosec // campos curtos, validados antes
		mac.Write(size[:])
		mac.Write([]byte(f))
	}
	return mac.Sum(nil)
}
