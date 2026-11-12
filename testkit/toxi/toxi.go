// Package toxi cria, para um teste, um proxy do Toxiproxy na frente de uma dependência real
// (Redis, um serviço gRPC...) e injeta falhas de rede nele: dependência fora do ar, latência,
// conexão cortada (ADR-0003). Fala com a API HTTP do Toxiproxy só com a biblioteca padrão.
//
// Configuração por ambiente (o contêiner tools já a define):
//
//	TEST_TOXIPROXY_URL   API do Toxiproxy (ex.: http://toxiproxy:8474)
//	TEST_TOXIPROXY_HOST  host em que os proxies escutam, visto pelo teste (ex.: toxiproxy)
package toxi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// Proxy é um proxy criado para um teste e removido no fim dele.
type Proxy struct {
	api  string
	name string
	// Addr é o endereço (host:porta) pelo qual o teste deve falar com a dependência.
	Addr string
}

// New cria um proxy para upstream (host:porta, visto pelo Toxiproxy) e agenda a remoção. Pula
// o teste se o Toxiproxy não estiver configurado.
func New(t testing.TB, upstream string) *Proxy {
	t.Helper()
	api, host := os.Getenv("TEST_TOXIPROXY_URL"), os.Getenv("TEST_TOXIPROXY_HOST")
	if api == "" || host == "" {
		t.Skip("Toxiproxy não configurado (TEST_TOXIPROXY_*): rode pelo contêiner tools: " +
			"docker compose run --rm tools make test")
	}
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	p := &Proxy{api: api, name: "t_" + hex.EncodeToString(suffix)}

	// Porta 0: o Toxiproxy escolhe uma livre, então testes em paralelo não colidem.
	var created struct {
		Listen string `json:"listen"`
	}
	p.call(t, http.MethodPost, "/proxies", map[string]any{
		"name": p.name, "listen": "0.0.0.0:0", "upstream": upstream, "enabled": true,
	}, &created)
	_, port, err := net.SplitHostPort(created.Listen)
	if err != nil {
		t.Fatalf("toxi: endereço do proxy %q: %v", created.Listen, err)
	}
	p.Addr = net.JoinHostPort(host, port)
	t.Cleanup(func() { p.call(t, http.MethodDelete, "/proxies/"+p.name, nil, nil) })
	return p
}

// Disable derruba o proxy: conexões abertas são fechadas e novas são recusadas, como se a
// dependência tivesse caído.
func (p *Proxy) Disable(t testing.TB) {
	t.Helper()
	p.call(t, http.MethodPost, "/proxies/"+p.name, map[string]any{"enabled": false}, nil)
}

// Enable religa o proxy (a dependência volta).
func (p *Proxy) Enable(t testing.TB) {
	t.Helper()
	p.call(t, http.MethodPost, "/proxies/"+p.name, map[string]any{"enabled": true}, nil)
}

// Latency atrasa cada resposta da dependência em d, até o fim do teste ou até RemoveToxics.
func (p *Proxy) Latency(t testing.TB, d time.Duration) {
	t.Helper()
	p.call(t, http.MethodPost, "/proxies/"+p.name+"/toxics", map[string]any{
		"name": "latency", "type": "latency", "stream": "downstream", "toxicity": 1,
		"attributes": map[string]any{"latency": d.Milliseconds()},
	}, nil)
}

// RemoveToxics tira as falhas injetadas por Latency.
func (p *Proxy) RemoveToxics(t testing.TB) {
	t.Helper()
	p.call(t, http.MethodDelete, "/proxies/"+p.name+"/toxics/latency", nil, nil)
}

func (p *Proxy) call(t testing.TB, method, path string, body, out any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("toxi: %v", err)
		}
		r = bytes.NewReader(b)
	}
	// O Cleanup roda depois de o contexto do teste ser cancelado: usa um contexto próprio.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, p.api+path, r)
	if err != nil {
		t.Fatalf("toxi: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("toxi: %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		t.Fatalf("toxi: %s %s respondeu %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("toxi: %s %s: %v", method, path, fmt.Errorf("resposta ilegível: %w", err))
		}
	}
}
