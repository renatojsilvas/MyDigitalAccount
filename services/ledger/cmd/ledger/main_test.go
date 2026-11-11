package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	pingv1 "conta.local/gen/go/conta/ping/v1"
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

func TestRunConfigErrorFailsFast(t *testing.T) {
	err := run(t.Context(), envFrom(map[string]string{"LOG_LEVEL": "inválido"}), &syncBuffer{})
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("esperava erro de configuração, veio %v", err)
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

func TestRunBootsServesAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, envFrom(map[string]string{
			"ENVIRONMENT": "test", "LEDGER_GRPC_ADDR": "127.0.0.1:0", "LEDGER_ADMIN_ADDR": "127.0.0.1:0",
			"SHUTDOWN_TIMEOUT": "2s",
		}), out)
	}()
	grpcAddr, adminAddr := startAddrs(t, out)

	for _, path := range []string{"/healthz", "/readyz"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+adminAddr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, resp.StatusCode)
		}
	}

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("encerramento normal devolveu erro: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serviço não encerrou após cancelar o contexto")
	}
	if !strings.Contains(out.String(), "serviço encerrado") {
		t.Errorf("log de encerramento ausente:\n%s", out.String())
	}
}
