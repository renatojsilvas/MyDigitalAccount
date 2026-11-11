package config_test

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"conta.local/ledger/internal/platform/config"
)

func lookupFrom(env map[string]string) config.Lookup {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

// creds acrescenta as credenciais do Postgres, que são obrigatórias.
func creds(env map[string]string) map[string]string {
	env["POSTGRES_USER"], env["POSTGRES_PASSWORD"] = "u", "p"
	return env
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config.Config
		wantErr []string
	}{
		{
			name: "padrões",
			env:  creds(map[string]string{}),
			want: config.Config{
				Environment: "development", LogLevel: slog.LevelInfo,
				GRPCAddr: ":50051", AdminAddr: ":8081", ShutdownTimeout: 10 * time.Second,
				Postgres: config.Postgres{Host: "postgres", Port: 5432, User: "u", Password: "p", Database: "ledger"},
			},
		},
		{
			name: "valores explícitos",
			env: creds(map[string]string{
				"ENVIRONMENT": "production", "LOG_LEVEL": "debug", "LEDGER_GRPC_ADDR": ":1",
				"LEDGER_ADMIN_ADDR": ":2", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317", "SHUTDOWN_TIMEOUT": "3s",
				"POSTGRES_HOST": "db", "POSTGRES_PORT": "6432", "LEDGER_DB_NAME": "outro",
			}),
			want: config.Config{
				Environment: "production", LogLevel: slog.LevelDebug, GRPCAddr: ":1",
				AdminAddr: ":2", OTLPEndpoint: "http://c:4317", ShutdownTimeout: 3 * time.Second,
				Postgres: config.Postgres{Host: "db", Port: 6432, User: "u", Password: "p", Database: "outro"},
			},
		},
		{
			name:    "ambiente inválido",
			env:     creds(map[string]string{"ENVIRONMENT": "staging"}),
			wantErr: []string{"ENVIRONMENT"},
		},
		{
			name:    "nível de log inválido",
			env:     creds(map[string]string{"LOG_LEVEL": "barulhento"}),
			wantErr: []string{"LOG_LEVEL"},
		},
		{
			name:    "timeout não positivo",
			env:     creds(map[string]string{"SHUTDOWN_TIMEOUT": "0s"}),
			wantErr: []string{"SHUTDOWN_TIMEOUT"},
		},
		{
			name:    "sem credenciais do Postgres",
			env:     map[string]string{},
			wantErr: []string{"POSTGRES_USER", "POSTGRES_PASSWORD"},
		},
		{
			name:    "porta do Postgres inválida",
			env:     creds(map[string]string{"POSTGRES_PORT": "70000"}),
			wantErr: []string{"POSTGRES_PORT"},
		},
		{
			name:    "reporta todos os erros juntos",
			env:     map[string]string{"ENVIRONMENT": "x", "LOG_LEVEL": "y", "SHUTDOWN_TIMEOUT": "z"},
			wantErr: []string{"ENVIRONMENT", "LOG_LEVEL", "SHUTDOWN_TIMEOUT"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.Load(lookupFrom(tt.env))
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("esperava erro contendo %v, veio nil", tt.wantErr)
				}
				for _, w := range tt.wantErr {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("erro %q não menciona %s", err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
