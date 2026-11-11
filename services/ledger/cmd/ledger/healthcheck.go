package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"conta.local/ledger/internal/platform/config"
)

const healthcheckTimeout = 2 * time.Second

// runHealthcheck consulta o /readyz do próprio processo, na porta admin. Existe porque a
// imagem final (distroless) não tem shell nem curl: o HEALTHCHECK do Docker chama
// `/ledger healthcheck`. Sai com erro se o serviço não estiver pronto.
func runHealthcheck(ctx context.Context, lookup config.Lookup) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("endereço admin %q inválido: %w", cfg.AdminAddr, err)
	}
	// Endereço de escuta curinga (":8081", "0.0.0.0") não é destino: consulta o loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/readyz"

	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("consultando %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s respondeu %d", url, resp.StatusCode)
	}
	return nil
}
