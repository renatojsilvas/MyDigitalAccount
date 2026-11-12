package healthcheck_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"conta.local/platform/healthcheck"
)

func TestProbe(t *testing.T) {
	serve := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		t.Cleanup(srv.Close)
		_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
		return port
	}
	closed := func() string {
		var lc net.ListenConfig
		lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, port, _ := net.SplitHostPort(lis.Addr().String())
		_ = lis.Close()
		return port
	}

	tests := []struct {
		name    string
		addr    func() string
		wantErr bool
	}{
		{"pronto (200), endereço curinga", func() string { return ":" + serve(http.StatusOK) }, false},
		{"pronto (200), 0.0.0.0", func() string { return "0.0.0.0:" + serve(http.StatusOK) }, false},
		{"não pronto (503)", func() string { return ":" + serve(http.StatusServiceUnavailable) }, true},
		{"nada escutando", func() string { return "127.0.0.1:" + closed() }, true},
		{"endereço sem porta", func() string { return "semporta" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := healthcheck.Probe(t.Context(), tt.addr())
			if (err != nil) != tt.wantErr {
				t.Fatalf("erro = %v, esperado erro = %v", err, tt.wantErr)
			}
		})
	}
}
