// Package pgbootstrap testa o script de provisionamento dos bancos (deploy/postgres/bootstrap.sh)
// contra a instância de teste do Postgres, com o psql da imagem tools. Os nomes de serviço são
// únicos por teste, então os papéis reais (ledger_*, spi_*) nunca são tocados.
package pgbootstrap_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/testkit/testpg"
)

// script é o caminho fixo do script versionado, relativo a este pacote.
const script = "../../deploy/postgres/bootstrap.sh"

type fixture struct {
	cfg   testpg.Config
	admin *pgxpool.Pool
	svc   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg, ok := testpg.ConfigFromEnv(os.LookupEnv)
	if !ok {
		t.Skip("instância de teste do Postgres não configurada (TEST_POSTGRES_*)")
	}
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql não encontrado: rode pelo contêiner tools")
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	f := &fixture{cfg: cfg, svc: "bt" + hex.EncodeToString(suffix)}

	pc, err := cfg.PoolConfig("postgres")
	if err != nil {
		t.Fatal(err)
	}
	f.admin, err = pgxpool.NewWithConfig(t.Context(), pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, sql := range []string{
			"DROP DATABASE IF EXISTS " + pgx.Identifier{f.svc}.Sanitize() + " WITH (FORCE)",
			"DROP ROLE IF EXISTS " + pgx.Identifier{f.svc + "_owner"}.Sanitize(),
			"DROP ROLE IF EXISTS " + pgx.Identifier{f.svc + "_app"}.Sanitize(),
		} {
			if _, err := f.admin.Exec(ctx, sql); err != nil {
				t.Errorf("limpeza: %v", err)
			}
		}
		f.admin.Close()
	})
	return f
}

// run executa o script como o job do compose: superusuário pelas variáveis PG* e as senhas do
// serviço pelas variáveis <SVC>_DB_*.
func (f *fixture) run(t *testing.T, ownerPassword, appPassword string) (string, error) {
	t.Helper()
	upper := strings.ToUpper(f.svc)
	cmd := exec.CommandContext(t.Context(), "sh", script)
	cmd.Env = append(os.Environ(),
		"PGHOST="+f.cfg.Host, "PGPORT="+strconv.Itoa(int(f.cfg.Port)),
		"PGUSER="+f.cfg.User, "PGPASSWORD="+f.cfg.Password,
		"BOOTSTRAP_SERVICES="+f.svc,
		upper+"_DB_OWNER_PASSWORD="+ownerPassword, upper+"_DB_APP_PASSWORD="+appPassword,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// catalog resume o que o script cria: papéis, dono do banco e ACL do banco.
func (f *fixture) catalog(t *testing.T) string {
	t.Helper()
	var roles, db string
	err := f.admin.QueryRow(t.Context(), `
		SELECT string_agg(format('%s login=%s super=%s createdb=%s', rolname, rolcanlogin, rolsuper, rolcreatedb), '; ' ORDER BY rolname)
		  FROM pg_roles WHERE rolname IN ($1, $2)`, f.svc+"_owner", f.svc+"_app").Scan(&roles)
	if err != nil {
		t.Fatal(err)
	}
	err = f.admin.QueryRow(t.Context(), `
		SELECT format('%s owner=%s acl=%s', datname, pg_get_userbyid(datdba), datacl::text)
		  FROM pg_database WHERE datname = $1`, f.svc).Scan(&db)
	if err != nil {
		t.Fatal(err)
	}
	return roles + " | " + db
}

// canConnect diz se o papel consegue conectar no banco do serviço com a senha dada.
func (f *fixture) canConnect(t *testing.T, role, password string) error {
	t.Helper()
	c := f.cfg
	c.User, c.Password = role, password
	pc, err := c.PoolConfig(f.svc)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), pc.ConnConfig)
	if err != nil {
		return err
	}
	return conn.Close(t.Context())
}

func TestBootstrapTwiceIsIdempotent(t *testing.T) {
	f := newFixture(t)
	if out, err := f.run(t, "owner-1", "app-1"); err != nil {
		t.Fatalf("primeira execução: %v\n%s", err, out)
	}
	first := f.catalog(t)
	if out, err := f.run(t, "owner-1", "app-1"); err != nil {
		t.Fatalf("segunda execução: %v\n%s", err, out)
	}
	if second := f.catalog(t); second != first {
		t.Fatalf("a segunda execução mudou o catálogo:\n antes: %s\n depois: %s", first, second)
	}

	want := fmt.Sprintf("%[1]s_app login=t super=f createdb=f; %[1]s_owner login=t super=f createdb=f | %[1]s owner=%[1]s_owner", f.svc)
	if !strings.HasPrefix(first, want) {
		t.Fatalf("catálogo = %s\nwant prefixo %s", first, want)
	}
	// ACL: o dono tem tudo, o app só CONNECT, e PUBLIC (entrada "=...") nada.
	acl := first[strings.Index(first, "acl="):]
	if !strings.Contains(acl, f.svc+"_app=c/") || strings.Contains(acl, "{=") || strings.Contains(acl, ",=") {
		t.Fatalf("ACL do banco = %s; want CONNECT só para o app e nada para PUBLIC", acl)
	}
	if err := f.canConnect(t, f.svc+"_app", "app-1"); err != nil {
		t.Fatalf("o app não conecta com a senha do bootstrap: %v", err)
	}
}

// O volume de desenvolvimento já tem o banco e os papéis do ledger, criados pelo antigo script
// de init: o job precisa funcionar em cima dele, sem apagar nada, e reaplicar as senhas.
func TestBootstrapOverExistingDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for _, sql := range []string{
		"CREATE ROLE " + pgx.Identifier{f.svc + "_owner"}.Sanitize() + " LOGIN PASSWORD 'antiga'",
		"CREATE ROLE " + pgx.Identifier{f.svc + "_app"}.Sanitize() + " LOGIN PASSWORD 'antiga'",
		"CREATE DATABASE " + pgx.Identifier{f.svc}.Sanitize() + " OWNER " + pgx.Identifier{f.svc + "_owner"}.Sanitize(),
	} {
		if _, err := f.admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	// Um dado gravado antes, que não pode sumir.
	pc, _ := f.cfg.PoolConfig(f.svc)
	conn, err := pgx.ConnectConfig(ctx, pc.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE marker (v int); INSERT INTO marker VALUES (42)"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)

	if out, err := f.run(t, "owner-nova", "app-nova"); err != nil {
		t.Fatalf("bootstrap sobre banco existente: %v\n%s", err, out)
	}
	if err := f.canConnect(t, f.svc+"_app", "app-nova"); err != nil {
		t.Fatalf("a senha nova não foi aplicada: %v", err)
	}
	if err := f.canConnect(t, f.svc+"_app", "antiga"); err == nil {
		t.Fatal("a senha antiga continua valendo")
	}
	conn, err = pgx.ConnectConfig(ctx, pc.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var v int
	if err := conn.QueryRow(ctx, "SELECT v FROM marker").Scan(&v); err != nil || v != 42 {
		t.Fatalf("o dado anterior sumiu: %v, %v", v, err)
	}
}

func TestBootstrapRejectsMissingConfig(t *testing.T) {
	f := newFixture(t)
	if out, err := f.run(t, "", "app"); err == nil || !strings.Contains(out, "_DB_OWNER_PASSWORD") {
		t.Fatalf("sem a senha do owner: err=%v saída=%s", err, out)
	}
	cmd := exec.CommandContext(t.Context(), "sh", script)
	cmd.Env = append(os.Environ(), "BOOTSTRAP_SERVICES=Inválido;DROP")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "nome de serviço inválido") {
		t.Fatalf("nome inválido: err=%v saída=%s", err, out)
	}
}
