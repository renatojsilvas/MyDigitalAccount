package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	antifraudev1 "conta.local/gen/go/conta/antifraude/v1"
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

// O serviço inteiro, contra o Redis real: sobe, avalia pelo gRPC, responde às sondas e encerra
// com graça quando o contexto é cancelado.
func TestRunServesAndShutsDown(t *testing.T) {
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		t.Skip("Redis de teste não configurado (TEST_REDIS_ADDR)")
	}
	env := map[string]string{
		"ENVIRONMENT": "test", "ANTIFRAUDE_GRPC_ADDR": "127.0.0.1:0", "ANTIFRAUDE_ADMIN_ADDR": "127.0.0.1:0",
		"SHUTDOWN_TIMEOUT": "2s", "REDIS_ADDR": redisAddr,
		// Este teste prova a montagem, não o prazo: com 50 ms, a primeira chamada (conexão fria e
		// carga do script) pode degradar sob CPU disputada. O prazo é provado em redisvelocity.
		"ANTIFRAUDE_REDIS_TIMEOUT": "1s",
	}
	ctx, cancel := context.WithCancel(t.Context())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- run(ctx, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, out) }()
	grpcAddr, adminAddr := startAddrs(t, out)

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := antifraudev1.NewAntifraudeServiceClient(conn).Evaluate(t.Context(), &antifraudev1.EvaluateRequest{
		TransferId: uuid.NewString(), AccountId: uuid.NewString(), AmountCents: 1_000,
	})
	if err != nil || resp.GetDecision() != antifraudev1.Decision_DECISION_APPROVE || resp.GetDegraded() {
		t.Fatalf("Evaluate = %v, %v; want aprovado, sem degradar", resp, err)
	}

	if err := runHealthcheck(t.Context(), func(k string) (string, bool) {
		if k == "ANTIFRAUDE_ADMIN_ADDR" {
			return adminAddr, true
		}
		return "", false
	}); err != nil {
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
	if !strings.Contains(out.String(), "serviço encerrado") {
		t.Fatalf("sem log de encerramento:\n%s", out.String())
	}
	var d net.Dialer
	if conn, err := d.DialContext(t.Context(), "tcp", adminAddr); err == nil {
		_ = conn.Close()
		t.Fatal("a porta admin continua aberta depois do encerramento")
	}
}

func TestRunFailsWithInvalidConfig(t *testing.T) {
	err := run(t.Context(), func(k string) (string, bool) {
		if k == "ANTIFRAUDE_FAULT_ERROR_RATE" {
			return "7", true
		}
		return "", false
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "ANTIFRAUDE_FAULT_") {
		t.Fatalf("err = %v, want erro de configuração", err)
	}
}
