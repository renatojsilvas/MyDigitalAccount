// Package pgtest prepara, para testes de integração, um banco do spi-fake migrado e um pool
// conectado como spi_app, o papel restrito do serviço, aberto pelo mesmo pg.Open de produção.
package pgtest

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/platform/pg"
	"conta.local/spi-fake/migrations"
	"conta.local/testkit/testpg"
)

// Papel do serviço na instância de teste. A senha é a mesma em todos os testes porque o
// papel é do cluster, compartilhado (testpg.EnsureRole).
const (
	AppRole     = "spi_app"
	AppPassword = "spi-app-test" //nolint:gosec // credencial só da instância de teste
)

// Migrated cria um banco isolado, aplica as migrations e devolve o banco e um pool como spi_app.
func Migrated(t testing.TB) (*testpg.Database, *pgxpool.Pool) {
	t.Helper()
	db := testpg.New(t)
	db.EnsureRole(t, AppRole, AppPassword)
	admin := Config(db)
	if _, err := pg.Migrate(t.Context(), admin, migrations.FS); err != nil {
		t.Fatalf("pgtest: migrations: %v", err)
	}
	app := admin
	app.User, app.Password = AppRole, AppPassword
	pool, err := pg.Open(t.Context(), app)
	if err != nil {
		t.Fatalf("pgtest: abrindo pool como %s: %v", AppRole, err)
	}
	t.Cleanup(pool.Close)
	return db, pool
}

// Config aponta para o banco de teste, com as credenciais do superusuário de teste.
func Config(db *testpg.Database) pg.Config {
	return pg.Config{
		Host: db.Config.Host, Port: db.Config.Port, User: db.Config.User,
		Password: db.Config.Password, Database: db.Name,
	}
}
