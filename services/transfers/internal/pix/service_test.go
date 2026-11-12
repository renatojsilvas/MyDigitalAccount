package pix_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"conta.local/transfers/internal/pix"
)

// fakeStore guarda o último aceite e simula a idempotência pela chave.
type fakeStore struct {
	accepted map[string]pix.AcceptRecord
	used     int64
}

func (f *fakeStore) Accept(_ context.Context, rec pix.AcceptRecord) (pix.Transfer, bool, error) {
	k := rec.Transfer.SourceAccountID.String() + "/" + string(rec.Key)
	if prev, ok := f.accepted[k]; ok {
		if !bytes.Equal(prev.RequestHMAC, rec.RequestHMAC) {
			return pix.Transfer{}, false, pix.ErrIdempotencyKeyReused
		}
		return prev.Transfer, false, nil
	}
	f.accepted[k] = rec
	return rec.Transfer, true, nil
}

func (f *fakeStore) Get(context.Context, uuid.UUID, uuid.UUID) (pix.Transfer, []pix.Change, error) {
	return pix.Transfer{}, nil, pix.ErrNotFound
}

func (f *fakeStore) UsedOn(context.Context, uuid.UUID, pix.Day) (int64, error) { return f.used, nil }

func command(t *testing.T, account uuid.UUID, cents int64, desc string) pix.StartCommand {
	t.Helper()
	cmd, err := pix.NewStartCommand("chave-1", account.String(), pix.KeyCPF, "12345678909", cents, desc)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func newService(t *testing.T, store *fakeStore, now time.Time, notified *[]uuid.UUID) *pix.Service {
	t.Helper()
	cal, err := pix.NewCalendar()
	if err != nil {
		t.Fatal(err)
	}
	return pix.NewService(store, cal, []byte("segredo-de-teste-com-32-bytes-ok"), 200_000,
		pix.WithClock(func() time.Time { return now }),
		pix.WithNotify(func(id uuid.UUID) { *notified = append(*notified, id) }))
}

func TestStart(t *testing.T) {
	store := &fakeStore{accepted: map[string]pix.AcceptRecord{}}
	var notified []uuid.UUID
	now := time.Date(2026, 11, 13, 2, 59, 0, 0, time.UTC) // 23:59 em São Paulo
	svc := newService(t, store, now, &notified)
	account := uuid.New()

	got, err := svc.Start(t.Context(), command(t, account, 5_000, "aluguel"))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != pix.StateAccepted || got.SettlementID == (uuid.UUID{}) || got.ID == got.SettlementID ||
		got.LimitDay != (pix.Day{Year: 2026, Month: 11, D: 12}) || got.CreatedAt != now {
		t.Fatalf("Pix aceito = %+v", got)
	}
	if len(notified) != 1 || notified[0] != got.ID {
		t.Fatalf("runner avisado = %v, want [%s]", notified, got.ID)
	}

	// Mesma chave, mesmo pedido: mesmo Pix, sem novo aviso.
	again, err := svc.Start(t.Context(), command(t, account, 5_000, "aluguel"))
	if err != nil || again.ID != got.ID || len(notified) != 1 {
		t.Fatalf("repetição = %+v, %v (avisos %d)", again, err, len(notified))
	}
	// Mesma chave, outro valor: recusa.
	if _, err := svc.Start(t.Context(), command(t, account, 5_001, "aluguel")); !errors.Is(err, pix.ErrIdempotencyKeyReused) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
	}
	// O HMAC não contém a chave Pix em claro (nem o CPF em hexa) e tem 32 bytes.
	for _, rec := range store.accepted {
		if len(rec.RequestHMAC) != 32 || bytes.Contains(rec.RequestHMAC, []byte("12345678909")) {
			t.Fatalf("HMAC suspeito: %x", rec.RequestHMAC)
		}
	}
}

func TestLimits(t *testing.T) {
	store := &fakeStore{used: 150_000}
	var notified []uuid.UUID
	now := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	l, err := newService(t, store, now, &notified).Limits(t.Context(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if l.DailyCents != 200_000 || l.UsedCents != 150_000 || l.AvailableCents != 50_000 ||
		!l.ResetsAt.Equal(time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("limites = %+v", l)
	}
}

func TestNewStartCommandValidation(t *testing.T) {
	id := uuid.NewString()
	tests := []struct {
		name, key, account, cpf string
		amount                  int64
		desc, field             string
	}{
		{"sem chave", "", id, "12345678909", 1, "", "idempotency_key"},
		{"conta inválida", "k", "x", "12345678909", 1, "", "account_id"},
		{"CPF inválido", "k", id, "123", 1, "", "destination_key.value"},
		{"valor zero", "k", id, "12345678909", 0, "", "amount_cents"},
		{"descrição com controle", "k", id, "12345678909", 1, "a\x00b", "description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pix.NewStartCommand(tt.key, tt.account, pix.KeyCPF, tt.cpf, tt.amount, tt.desc)
			var verr *pix.ValidationError
			if !errors.As(err, &verr) || verr.Field != tt.field {
				t.Fatalf("err = %v, want campo %s", err, tt.field)
			}
		})
	}
}
