package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A sonda em si é testada em platform/healthcheck; aqui, só a ligação com a configuração.
func TestRunHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{"usa LEDGER_ADMIN_ADDR", map[string]string{"LEDGER_ADMIN_ADDR": ":" + port}, false},
		{"configuração inválida", map[string]string{"LOG_LEVEL": "x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// O Postgres não é consultado pelo healthcheck, mas a configuração o exige.
			tt.env["LEDGER_DB_USER"], tt.env["LEDGER_DB_PASSWORD"] = "u", "p"
			err := runHealthcheck(t.Context(), func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok })
			if (err != nil) != tt.wantErr {
				t.Fatalf("erro = %v, esperado erro = %v", err, tt.wantErr)
			}
		})
	}
}
