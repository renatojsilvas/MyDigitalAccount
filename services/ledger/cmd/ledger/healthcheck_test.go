package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunHealthcheck(t *testing.T) {
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
		env     func() map[string]string
		wantErr bool
	}{
		{"pronto (200), endereço curinga", func() map[string]string { return map[string]string{"LEDGER_ADMIN_ADDR": ":" + serve(http.StatusOK)} }, false},
		{"não pronto (503)", func() map[string]string {
			return map[string]string{"LEDGER_ADMIN_ADDR": ":" + serve(http.StatusServiceUnavailable)}
		}, true},
		{"nada escutando", func() map[string]string { return map[string]string{"LEDGER_ADMIN_ADDR": "127.0.0.1:" + closed()} }, true},
		{"configuração inválida", func() map[string]string { return map[string]string{"LOG_LEVEL": "x"} }, true},
		{"endereço sem porta", func() map[string]string { return map[string]string{"LEDGER_ADMIN_ADDR": "semporta"} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := tt.env()
			// O Postgres não é consultado pelo healthcheck, mas a configuração o exige.
			env["LEDGER_DB_USER"], env["LEDGER_DB_PASSWORD"] = "u", "p"
			err := runHealthcheck(t.Context(), func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			if (err != nil) != tt.wantErr {
				t.Fatalf("erro = %v, esperado erro = %v", err, tt.wantErr)
			}
		})
	}
}
