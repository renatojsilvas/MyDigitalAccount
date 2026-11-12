package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"conta.local/spi-fake/internal/postgres"
	"conta.local/spi-fake/internal/postgres/pgtest"
	"conta.local/spi-fake/internal/settlement"
)

func newSettlement(status settlement.Status) settlement.Settlement {
	st := settlement.Settlement{
		Request: settlement.Request{
			ID: uuid.New(), TransferID: uuid.New(), Payer: uuid.New(), Payee: uuid.New(), AmountCents: 12_345,
		},
		Status:    status,
		DecidedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if status == settlement.Rejected {
		st.RejectReason = settlement.RejectSimulated
	}
	return st
}

func TestInsertAndGet(t *testing.T) {
	_, pool := pgtest.Migrated(t)
	s := postgres.NewStore(pool)
	for _, status := range []settlement.Status{settlement.Settled, settlement.Rejected} {
		st := newSettlement(status)
		inserted, err := s.Insert(t.Context(), st)
		if err != nil || !inserted {
			t.Fatalf("Insert = %v, %v", inserted, err)
		}
		got, err := s.Get(t.Context(), st.ID)
		if err != nil || got != st {
			t.Fatalf("Get = %+v, %v; want %+v", got, err, st)
		}
	}
	if _, err := s.Get(t.Context(), uuid.New()); !errors.Is(err, settlement.ErrNotFound) {
		t.Fatalf("ID inexistente: %v, want ErrNotFound", err)
	}
}

// O primeiro resultado vale para sempre: um segundo Insert do mesmo ID não sobrescreve.
func TestInsertNeverOverwrites(t *testing.T) {
	_, pool := pgtest.Migrated(t)
	s := postgres.NewStore(pool)
	first := newSettlement(settlement.Settled)
	if _, err := s.Insert(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Status, second.RejectReason = settlement.Rejected, settlement.RejectSimulated
	inserted, err := s.Insert(t.Context(), second)
	if err != nil || inserted {
		t.Fatalf("segundo Insert = %v, %v; want não inserido", inserted, err)
	}
	if got, _ := s.Get(t.Context(), first.ID); got.Status != settlement.Settled {
		t.Fatalf("status = %v, want o primeiro (settled)", got.Status)
	}
}

// Pedidos concorrentes com o mesmo ID (retries do transfers): exatamente um grava.
func TestConcurrentInsertsSameID(t *testing.T) {
	_, pool := pgtest.Migrated(t)
	s := postgres.NewStore(pool)
	st := newSettlement(settlement.Settled)
	const n = 20
	var wg sync.WaitGroup
	results := make([]bool, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() { results[i], errs[i] = s.Insert(context.Background(), st) })
	}
	wg.Wait()
	var inserted int
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i] {
			inserted++
		}
	}
	if inserted != 1 {
		t.Fatalf("%d gravações, want 1", inserted)
	}
}

// O resultado é imutável também por privilégio: o papel do serviço não altera nem apaga.
func TestServiceRoleCannotRewriteHistory(t *testing.T) {
	_, pool := pgtest.Migrated(t)
	for _, sql := range []string{"UPDATE settlements SET status = 'rejected'", "DELETE FROM settlements"} {
		_, err := pool.Exec(t.Context(), sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: err = %v, want 42501 (sem privilégio)", sql, err)
		}
	}
}
