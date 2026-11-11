package postgres_test

import (
	"net"
	"strings"
	"testing"

	"conta.local/ledger/internal/platform/config"
	"conta.local/ledger/internal/platform/postgres"
	"conta.local/testkit/testpg"
)

func fromTestDB(db *testpg.Database) config.Postgres {
	return config.Postgres{
		Host: db.Config.Host, Port: db.Config.Port, User: db.Config.User,
		Password: db.Config.Password, Database: db.Name,
	}
}

func TestOpen(t *testing.T) {
	db := testpg.New(t)
	good := fromTestDB(db)

	wrongPassword := good
	wrongPassword.Password = "senha-errada"
	unknownDB := good
	unknownDB.Database = "nao_existe"

	// Porta em que nada escuta.
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := uint16(lis.Addr().(*net.TCPAddr).Port) //nolint:gosec // porta de TCP cabe em uint16
	_ = lis.Close()
	unreachable := good
	unreachable.Host, unreachable.Port = "127.0.0.1", closedPort

	tests := []struct {
		name    string
		cfg     config.Postgres
		wantErr bool
	}{
		{"banco acessível", good, false},
		{"senha errada", wrongPassword, true},
		{"banco inexistente", unknownDB, true},
		{"servidor inacessível", unreachable, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := postgres.Open(t.Context(), tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("erro = %v, esperado erro = %v", err, tt.wantErr)
			}
			if err != nil {
				if got := err.Error(); tt.cfg.Password != "" && strings.Contains(got, tt.cfg.Password) {
					t.Errorf("o erro vazou a senha: %s", got)
				}
				return
			}
			defer pool.Close()
			var one int
			if err := pool.QueryRow(t.Context(), "SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Errorf("SELECT 1: %d, %v", one, err)
			}
		})
	}
}
