// Package healthcheck é a sonda usada pelo HEALTHCHECK das imagens finais (distroless), que
// não têm shell nem curl: o próprio binário do serviço tem um subcomando `healthcheck` que
// consulta o /readyz da sua porta admin.
package healthcheck

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Timeout limita a sonda inteira.
const Timeout = 2 * time.Second

// Probe consulta o /readyz em adminAddr (o mesmo endereço de escuta da porta admin) e devolve
// erro se o serviço não responder 200 dentro do Timeout.
func Probe(ctx context.Context, adminAddr string) error {
	host, port, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return fmt.Errorf("endereço admin %q inválido: %w", adminAddr, err)
	}
	// Endereço de escuta curinga (":8081", "0.0.0.0") não é destino: consulta o loopback.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/readyz"

	ctx, cancel := context.WithTimeout(ctx, Timeout)
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
