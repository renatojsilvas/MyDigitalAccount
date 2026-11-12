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

	transfersv1 "conta.local/gen/go/conta/transfers/v1"
	"conta.local/testkit/testpg"
	"conta.local/transfers/internal/postgres/pgtest"
	"conta.local/transfers/migrations"
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
		"ENVIRONMENT": "test", "TRANSFERS_GRPC_ADDR": "127.0.0.1:0", "TRANSFERS_ADMIN_ADDR": "127.0.0.1:0", "SHUTDOWN_TIMEOUT": "2s",
		"POSTGRES_HOST": db.Config.Host, "POSTGRES_PORT": strconv.Itoa(int(db.Config.Port)),
		"TRANSFERS_DB_USER": user, "TRANSFERS_DB_PASSWORD": password, "TRANSFERS_DB_NAME": db.Name,
		"TRANSFERS_REQUEST_HASH_KEY": "segredo-de-teste-com-32-bytes-ok",
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

// O serviço inteiro: sobe com o papel restrito, aceita um Pix pelo gRPC, responde à sonda e encerra.
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
	client := transfersv1.NewTransfersServiceClient(conn)
	resp, err := client.StartPix(t.Context(), &transfersv1.StartPixRequest{
		IdempotencyKey: uuid.NewString(), AccountId: uuid.NewString(), AmountCents: 100,
		DestinationKey: &transfersv1.PixKey{Type: transfersv1.PixKeyType_PIX_KEY_TYPE_CPF, Value: "12345678909"},
	})
	if err != nil || resp.GetPix().GetState() != transfersv1.PixState_PIX_STATE_ACCEPTED {
		t.Fatalf("StartPix = %v, %v", resp, err)
	}
	if err := runHealthcheck(t.Context(), lookup(map[string]string{
		"TRANSFERS_ADMIN_ADDR": adminAddr, "TRANSFERS_DB_USER": "u", "TRANSFERS_DB_PASSWORD": "p",
		"TRANSFERS_REQUEST_HASH_KEY": "segredo-de-teste-com-32-bytes-ok",
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
