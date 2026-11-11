package testpg_test

import (
	"context"
	"testing"
	"time"

	"conta.local/testkit/testpg"
)

func TestConfigFromEnv(t *testing.T) {
	full := map[string]string{
		"TEST_POSTGRES_HOST": "h", "TEST_POSTGRES_USER": "u", "TEST_POSTGRES_PASSWORD": "p",
	}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for kk, vv := range full {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	without := func(k string) map[string]string {
		m := with(k, "")
		delete(m, k)
		return m
	}
	tests := []struct {
		name   string
		env    map[string]string
		want   testpg.Config
		wantOK bool
	}{
		{"completo, porta padrão", full, testpg.Config{Host: "h", Port: 5432, User: "u", Password: "p"}, true},
		{"porta explícita", with("TEST_POSTGRES_PORT", "6543"), testpg.Config{Host: "h", Port: 6543, User: "u", Password: "p"}, true},
		{"porta inválida", with("TEST_POSTGRES_PORT", "abc"), testpg.Config{}, false},
		{"porta fora do intervalo", with("TEST_POSTGRES_PORT", "70000"), testpg.Config{}, false},
		{"sem host", without("TEST_POSTGRES_HOST"), testpg.Config{}, false},
		{"sem usuário", without("TEST_POSTGRES_USER"), testpg.Config{}, false},
		{"sem senha", without("TEST_POSTGRES_PASSWORD"), testpg.Config{}, false},
		{"vazio", map[string]string{}, testpg.Config{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := testpg.ConfigFromEnv(func(k string) (string, bool) { v, ok := tt.env[k]; return v, ok })
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("got (%+v, %v), want (%+v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func count(t *testing.T, db *testpg.Database, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Pool.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestDatabasesAreIsolated(t *testing.T) {
	a, b := testpg.New(t), testpg.New(t)
	if a.Name == b.Name {
		t.Fatalf("dois testes receberam o mesmo banco: %s", a.Name)
	}
	if _, err := a.Pool.Exec(t.Context(), "CREATE TABLE contas (id int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pool.Exec(t.Context(), "INSERT INTO contas VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, a, "SELECT count(*) FROM contas"); n != 1 {
		t.Errorf("banco A: %d linhas", n)
	}
	// A tabela criada em A não existe em B.
	if _, err := b.Pool.Exec(t.Context(), "SELECT 1 FROM contas"); err == nil {
		t.Error("o banco B enxergou a tabela do banco A")
	}
}

func TestParallelTestsDoNotCollide(t *testing.T) {
	for i := range 6 {
		t.Run("paralelo", func(t *testing.T) {
			t.Parallel()
			db := testpg.New(t)
			// O mesmo nome de tabela em todos: só funciona se cada teste tem o seu banco.
			if _, err := db.Pool.Exec(t.Context(), "CREATE TABLE t (n int)"); err != nil {
				t.Fatalf("teste %d: %v", i, err)
			}
		})
	}
}

func TestCleanupRemovesDatabase(t *testing.T) {
	observer := testpg.New(t)
	var name string
	t.Run("cria e descarta", func(t *testing.T) {
		name = testpg.New(t).Name
	})
	// O Cleanup do subteste já rodou: o banco não deve mais existir.
	if n := count(t, observer, "SELECT count(*) FROM pg_database WHERE datname = $1", name); n != 0 {
		t.Errorf("banco %s continuou existindo após o teste", name)
	}
}

func TestDropIsIdempotentAndForcesOpenConnections(t *testing.T) {
	db := testpg.New(t)
	// Conexão aberta e ocupada: sem WITH (FORCE) o DROP falharia.
	conn, err := db.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := db.Drop(ctx); err != nil {
		t.Fatalf("primeiro Drop: %v", err)
	}
	if err := db.Drop(ctx); err != nil {
		t.Fatalf("segundo Drop (idempotência): %v", err)
	}
}
