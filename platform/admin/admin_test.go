package admin_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"conta.local/platform/admin"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func listen(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return lis
}

func get(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec.Code
}

func TestProbes(t *testing.T) {
	tests := []struct {
		name     string
		checks   []admin.Check
		notReady bool
		path     string
		wantCode int
	}{
		{"healthz sempre 200", nil, false, "/healthz", http.StatusOK},
		{"healthz segue 200 durante desligamento", nil, true, "/healthz", http.StatusOK},
		{"readyz sem dependências", nil, false, "/readyz", http.StatusOK},
		{"readyz com dependência saudável", []admin.Check{{Name: "db", Fn: func(context.Context) error { return nil }}}, false, "/readyz", http.StatusOK},
		{"readyz com dependência falhando", []admin.Check{{Name: "db", Fn: func(context.Context) error { return errors.New("down") }}}, false, "/readyz", http.StatusServiceUnavailable},
		{"readyz durante desligamento", nil, true, "/readyz", http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := admin.New(discard(), tt.checks...)
			if tt.notReady {
				s.SetReady(false)
			}
			if got := get(t, s.Handler(), tt.path); got != tt.wantCode {
				t.Errorf("GET %s = %d, esperado %d", tt.path, got, tt.wantCode)
			}
		})
	}
}

func TestServeShutsDownOnCancel(t *testing.T) {
	lis := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- admin.New(discard()).Serve(ctx, lis, time.Second) }()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+lis.Addr().String()+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve devolveu erro no encerramento normal: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve não encerrou após cancelar o contexto")
	}
}

func TestServeForcesCloseWhenConnectionHangs(t *testing.T) {
	lis := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- admin.New(discard()).Serve(ctx, lis, 200*time.Millisecond) }()

	// Requisição incompleta: a conexão fica ativa e nunca termina sozinha.
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve devolveu erro; deveria fechar à força e encerrar: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve travou numa conexão pendurada")
	}
}
