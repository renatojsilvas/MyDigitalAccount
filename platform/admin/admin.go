// Package admin expõe /healthz e /readyz numa porta HTTP separada da do gRPC. Assim o
// healthcheck do Docker e as sondas não passam pelos interceptors de negócio (não geram
// trace nem inflam as métricas RED) e a porta pode ficar só na rede interna.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Check verifica uma dependência. Devolve erro se ela não estiver pronta.
type Check struct {
	Name string
	Fn   func(ctx context.Context) error
}

// Server responde às sondas de saúde.
type Server struct {
	logger  *slog.Logger
	checks  []Check
	ready   atomic.Bool
	timeout time.Duration
}

// New cria o servidor já pronto (readiness verdadeiro).
func New(logger *slog.Logger, checks ...Check) *Server {
	s := &Server{logger: logger, checks: checks, timeout: 2 * time.Second}
	s.ready.Store(true)
	return s
}

// SetReady liga ou desliga a prontidão. O desligamento gracioso a desliga primeiro, para
// o balanceador parar de mandar tráfego antes de o servidor gRPC parar.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Handler devolve as rotas.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.readyz)
	return mux
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "shutting_down"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var failed []string
	for _, c := range s.checks {
		if err := c.Fn(ctx); err != nil {
			s.logger.WarnContext(ctx, "dependência não está pronta", "dependency", c.Name, "error", err)
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "failed": failed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// Serve atende em lis até ctx ser cancelado e então encerra com graça, dentro de
// shutdownTimeout. Devolve nil no encerramento normal.
func (s *Server) Serve(ctx context.Context, lis net.Listener, shutdownTimeout time.Duration) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()

	select {
	case err := <-errCh:
		return fmt.Errorf("servidor admin: %w", err)
	case <-ctx.Done():
	}
	// ctx já foi cancelado; o desligamento precisa de um contexto novo, com prazo próprio.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	switch {
	case err == nil, errors.Is(err, http.ErrServerClosed):
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		// Uma conexão pendurada (sonda lenta) não pode travar o encerramento do serviço.
		s.logger.WarnContext(shutdownCtx, "admin: fechando conexões que não terminaram no prazo")
		_ = srv.Close()
		return nil
	default:
		return fmt.Errorf("encerrando servidor admin: %w", err)
	}
}
