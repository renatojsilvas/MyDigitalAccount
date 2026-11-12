package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/spi-fake/internal/postgres/pgtest"
	"conta.local/spi-fake/migrations"
	"conta.local/testkit/testpg"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func lookup(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func records(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			recs = append(recs, rec)
		}
	}
	return recs
}

func baseEnv(db *testpg.Database, user, password string) map[string]string {
	return map[string]string{
		"ENVIRONMENT": "test", "SPI_GRPC_ADDR": "127.0.0.1:0", "SPI_ADMIN_ADDR": "127.0.0.1:0", "SHUTDOWN_TIMEOUT": "2s",
		"POSTGRES_HOST": db.Config.Host, "POSTGRES_PORT": strconv.Itoa(int(db.Config.Port)),
		"SPI_DB_USER": user, "SPI_DB_PASSWORD": password, "SPI_DB_NAME": db.Name,
	}
}

// O job de migration aplica tudo na primeira vez e nada na segunda.
func TestRunMigrate(t *testing.T) {
	db := testpg.New(t)
	db.EnsureRole(t, pgtest.AppRole, pgtest.AppPassword)
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{float64(len(files)), 0} {
		var out bytes.Buffer
		if err := runMigrate(t.Context(), lookup(baseEnv(db, db.Config.User, db.Config.Password)), &out); err != nil {
			t.Fatalf("execução %d: %v\n%s", i+1, err, out.String())
		}
		recs := records(t, out.String())
		if last := recs[len(recs)-1]; last["msg"] != "migrations em dia" || last["applied"] != want {
			t.Fatalf("execução %d: %v, want %v aplicada(s)", i+1, last, want)
		}
	}
}

// O serviço inteiro: sobe com o papel restrito, liquida pelo gRPC, responde à sonda e encerra.
func TestRunServesAndShutsDown(t *testing.T) {
	db, _ := pgtest.Migrated(t)
	ctx, cancel := context.WithCancel(t.Context())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, lookup(baseEnv(db, pgtest.AppRole, pgtest.AppPassword)), out) }()

	var grpcAddr, adminAddr string
	for deadline := time.Now().Add(5 * time.Second); grpcAddr == "" && time.Now().Before(deadline); {
		for _, rec := range records(t, out.String()) {
			if rec["msg"] == "serviço iniciando" {
				grpcAddr, adminAddr = rec["grpc_addr"].(string), rec["admin_addr"].(string)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grpcAddr == "" {
		t.Fatalf("serviço não iniciou; saída:\n%s", out.String())
	}

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := spiv1.NewSpiServiceClient(conn).Settle(t.Context(), &spiv1.SettleRequest{
		SettlementId: uuid.NewString(), TransferId: uuid.NewString(),
		PayerAccountId: uuid.NewString(), PayeeAccountId: uuid.NewString(), AmountCents: 100,
	})
	if err != nil || resp.GetSettlement().GetStatus() != spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED {
		t.Fatalf("Settle = %v, %v", resp, err)
	}
	if err := runHealthcheck(t.Context(), lookup(map[string]string{
		"SPI_ADMIN_ADDR": adminAddr, "SPI_DB_USER": "u", "SPI_DB_PASSWORD": "p",
	})); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run terminou com erro: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run não encerrou a tempo")
	}
	var d net.Dialer
	if c, err := d.DialContext(t.Context(), "tcp", adminAddr); err == nil {
		_ = c.Close()
		t.Fatal("a porta admin continua aberta depois do encerramento")
	}
}
