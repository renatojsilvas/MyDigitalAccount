// Package pgtest prepara, para testes de integração, um banco do ledger migrado e um pool
// conectado como ledger_app, o papel restrito do serviço, aberto pelo mesmo pg.Open
// usado em produção.
package pgtest

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/ledger/internal/platform/config"
	"conta.local/ledger/migrations"
	"conta.local/platform/pg"
	"conta.local/testkit/testpg"
)

// Papel do serviço na instância de teste. A senha é a mesma em todos os testes porque o
// papel é do cluster, compartilhado (testpg.EnsureRole).
const (
	AppRole     = "ledger_app"
	AppPassword = "ledger-app-test" //nolint:gosec // credencial só da instância de teste
)

// DefaultLockTimeout é o lock_timeout usado quando o teste não pede outro.
const DefaultLockTimeout = time.Second

// Migrated cria um banco isolado, aplica as migrations como superusuário de teste e devolve
// o banco e um pool como ledger_app com o lock_timeout dado (zero usa DefaultLockTimeout).
func Migrated(t testing.TB, lockTimeout time.Duration) (*testpg.Database, *pgxpool.Pool) {
	t.Helper()
	if lockTimeout == 0 {
		lockTimeout = DefaultLockTimeout
	}
	db := testpg.New(t)
	db.EnsureRole(t, AppRole, AppPassword)

	admin := Config(db)
	if _, err := pg.Migrate(t.Context(), admin, migrations.FS); err != nil {
		t.Fatalf("pgtest: migrations: %v", err)
	}

	app := admin
	app.User, app.Password, app.LockTimeout = AppRole, AppPassword, lockTimeout
	pool, err := pg.Open(t.Context(), app)
	if err != nil {
		t.Fatalf("pgtest: abrindo pool como %s: %v", AppRole, err)
	}
	t.Cleanup(pool.Close)
	return db, pool
}

// Config devolve a configuração do ledger apontando para o banco de teste, com as
// credenciais do superusuário de teste.
func Config(db *testpg.Database) config.Postgres {
	return config.Postgres{
		Host: db.Config.Host, Port: db.Config.Port, User: db.Config.User,
		Password: db.Config.Password, Database: db.Name,
	}
}
