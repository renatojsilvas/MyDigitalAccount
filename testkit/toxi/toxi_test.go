package toxi_test

import (
	"bufio"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"conta.local/testkit/toxi"
)

// ping manda PING ao Redis de teste pelo endereço dado e devolve a resposta (ou o erro).
func ping(t *testing.T, addr string, timeout time.Duration) (string, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(t.Context(), "tcp", addr) //nolint:gosec // endereço do proxy de teste, não entrada externa
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line), err
}

func TestProxyInjectsFailures(t *testing.T) {
	upstream := os.Getenv("TEST_REDIS_ADDR")
	if upstream == "" {
		t.Skip("TEST_REDIS_ADDR não configurado")
	}
	p := toxi.New(t, upstream)

	if got, err := ping(t, p.Addr, 2*time.Second); err != nil || got != "+PONG" {
		t.Fatalf("pelo proxy = %q, %v; want +PONG", got, err)
	}

	p.Latency(t, 300*time.Millisecond)
	start := time.Now()
	if _, err := ping(t, p.Addr, 2*time.Second); err != nil || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("com latência: %v em %v; want resposta depois de 300ms", err, time.Since(start))
	}
	p.RemoveToxics(t)

	p.Disable(t)
	if got, err := ping(t, p.Addr, time.Second); err == nil {
		t.Fatalf("com o proxy desligado respondeu %q; want erro", got)
	}
	p.Enable(t)
	if got, err := ping(t, p.Addr, 2*time.Second); err != nil || got != "+PONG" {
		t.Fatalf("religado = %q, %v; want +PONG", got, err)
	}
}
