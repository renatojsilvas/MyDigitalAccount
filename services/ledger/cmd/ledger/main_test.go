package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pingv1 "conta.local/gen/go/conta/ping/v1"
	"conta.local/testkit/testpg"
)

// syncBuffer permite ler a saída do serviço enquanto ele ainda escreve.
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

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// ledgerEnv monta o ambiente do serviço apontando para o banco de teste isolado.
func ledgerEnv(db *testpg.Database) map[string]string {
	return map[string]string{
		"ENVIRONMENT": "test", "LEDGER_GRPC_ADDR": "127.0.0.1:0", "LEDGER_ADMIN_ADDR": "127.0.0.1:0",
		"SHUTDOWN_TIMEOUT": "2s",
		"POSTGRES_HOST":    db.Config.Host, "POSTGRES_PORT": strconv.Itoa(int(db.Config.Port)),
		"POSTGRES_USER": db.Config.User, "POSTGRES_PASSWORD": db.Config.Password, "LEDGER_DB_NAME": db.Name,
	}
}

// startAddrs espera o log "serviço iniciando" e devolve os endereços em que o serviço ouve.
func startAddrs(t *testing.T, out *syncBuffer) (grpcAddr, adminAddr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sc := bufio.NewScanner(strings.NewReader(out.String()))
		for sc.Scan() {
			var rec map[string]any
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec["msg"] == "serviço iniciando" {
				return rec["grpc_addr"].(string), rec["admin_addr"].(string)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serviço não iniciou a tempo; saída:\n%s", out.String())
	return "", ""
}

// running é um ledger em execução, com o encerramento controlável pelo teste.
type running struct {
	grpcAddr, adminAddr string
	out                 *syncBuffer
	stop                func() error
}

func startLedger(t *testing.T, env map[string]string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, envFrom(env), out) }()
	grpcAddr, adminAddr := startAddrs(t, out)

	r := &running{grpcAddr: grpcAddr, adminAddr: adminAddr, out: out}
	var once sync.Once
	var stopErr error
	r.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(5 * time.Second):
				t.Error("serviço não encerrou após cancelar o contexto")
			}
		})
		return stopErr
	}
	t.Cleanup(func() { _ = r.stop() })
	return r
}

func get(t *testing.T, addr, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	return resp.StatusCode, body.String()
}

func TestRunConfigErrorFailsFast(t *testing.T) {
	err := run(t.Context(), envFrom(map[string]string{"LOG_LEVEL": "inválido"}), &syncBuffer{})
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("esperava erro de configuração, veio %v", err)
	}
}

// Não precisa da instância de teste: aponta para uma porta onde nada escuta.
func TestRunFailsFastWhenPostgresIsUnreachable(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	err = run(t.Context(), envFrom(map[string]string{
		"ENVIRONMENT": "test", "LEDGER_GRPC_ADDR": "127.0.0.1:0", "LEDGER_ADMIN_ADDR": "127.0.0.1:0",
		"POSTGRES_HOST": "127.0.0.1", "POSTGRES_PORT": strconv.Itoa(port),
		"POSTGRES_USER": "u", "POSTGRES_PASSWORD": "senha-secreta",
	}), &syncBuffer{})
	if err == nil || !strings.Contains(err.Error(), "postgres") {
		t.Fatalf("esperava falha de conexão com o Postgres no boot, veio %v", err)
	}
	if strings.Contains(err.Error(), "senha-secreta") {
		t.Errorf("o erro vazou a senha: %v", err)
	}
}

func TestRunBootsServesAndShutsDown(t *testing.T) {
	db := testpg.New(t)
	r := startLedger(t, ledgerEnv(db))

	for _, path := range []string{"/healthz", "/readyz"} {
		if code, _ := get(t, r.adminAddr, path); code != http.StatusOK {
			t.Errorf("GET %s = %d", path, code)
		}
	}

	conn, err := grpc.NewClient(r.grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	pong, err := pingv1.NewPingServiceClient(conn).Ping(t.Context(), &pingv1.PingRequest{Message: "oi"})
	if err != nil || pong.GetService() != "ledger" || pong.GetMessage() != "oi" {
		t.Fatalf("ping: resp=%v err=%v", pong, err)
	}
	hc, err := healthpb.NewHealthClient(conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || hc.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("grpc.health.v1: resp=%v err=%v", hc, err)
	}

	if err := r.stop(); err != nil {
		t.Fatalf("encerramento normal devolveu erro: %v", err)
	}
	if !strings.Contains(r.out.String(), "serviço encerrado") {
		t.Errorf("log de encerramento ausente:\n%s", r.out.String())
	}
}

// O /readyz acompanha o Postgres; o /healthz (processo vivo) não. Derrubar o banco com o
// serviço no ar tira o ledger de "pronto" sem derrubar o processo.
func TestReadyzFollowsPostgres(t *testing.T) {
	db := testpg.New(t)
	r := startLedger(t, ledgerEnv(db))

	if code, _ := get(t, r.adminAddr, "/readyz"); code != http.StatusOK {
		t.Fatalf("readyz com o banco no ar = %d", code)
	}

	if err := db.Drop(t.Context()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var code int
	var body string
	for time.Now().Before(deadline) {
		if code, body = get(t, r.adminAddr, "/readyz"); code == http.StatusServiceUnavailable {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"postgres"`) {
		t.Fatalf("readyz sem o banco = %d %s; esperado 503 apontando o postgres", code, body)
	}
	if hz, _ := get(t, r.adminAddr, "/healthz"); hz != http.StatusOK {
		t.Errorf("healthz = %d; o processo continua vivo e deve responder 200", hz)
	}
}
