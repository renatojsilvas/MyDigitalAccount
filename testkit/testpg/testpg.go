// Package testpg entrega a cada teste um banco Postgres próprio e descartável, dentro da
// instância de teste do compose (ADR-0003, opção B).
//
// O isolamento é por banco, não por contêiner: cada teste cria `t_<aleatório>` e o remove
// ao terminar. Dois testes (ou dois pacotes, em paralelo) nunca enxergam os dados um do outro.
// Fora do compose (sem TEST_POSTGRES_HOST) o teste é pulado, com a instrução de como rodar.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const adminDatabase = "postgres"

// Config descreve como alcançar a instância de teste.
type Config struct {
	Host     string
	Port     uint16
	User     string
	Password string
}

// Lookup tem a assinatura de os.LookupEnv.
type Lookup func(string) (string, bool)

// ConfigFromEnv lê TEST_POSTGRES_HOST, _PORT (padrão 5432), _USER e _PASSWORD. O segundo
// retorno é falso quando a instância de teste não está configurada.
func ConfigFromEnv(lookup Lookup) (Config, bool) {
	get := func(k string) string { v, _ := lookup(k); return v }
	cfg := Config{
		Host:     get("TEST_POSTGRES_HOST"),
		User:     get("TEST_POSTGRES_USER"),
		Password: get("TEST_POSTGRES_PASSWORD"),
		Port:     5432,
	}
	if p := get("TEST_POSTGRES_PORT"); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return Config{}, false
		}
		cfg.Port = uint16(n)
	}
	if cfg.Host == "" || cfg.User == "" || cfg.Password == "" {
		return Config{}, false
	}
	return cfg, true
}

// PoolConfig monta a configuração do pool para o banco indicado. Os campos são atribuídos
// um a um (em vez de montar uma URL), então senhas com caracteres especiais não exigem escape.
func (c Config) PoolConfig(database string) (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, err
	}
	pc.ConnConfig.Host = c.Host
	pc.ConnConfig.Port = c.Port
	pc.ConnConfig.User = c.User
	pc.ConnConfig.Password = c.Password
	pc.ConnConfig.Database = database
	pc.ConnConfig.ConnectTimeout = 5 * time.Second
	return pc, nil
}

// Database é um banco criado para um teste.
type Database struct {
	Name   string
	Config Config
	Pool   *pgxpool.Pool
}

// New cria um banco novo e vazio, abre um pool nele e agenda a remoção para o fim do teste.
func New(t testing.TB) *Database {
	t.Helper()
	cfg, ok := ConfigFromEnv(os.LookupEnv)
	if !ok {
		t.Skip("instância de teste do Postgres não configurada (TEST_POSTGRES_*): " +
			"rode pelo contêiner tools: docker compose run --rm tools make test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	name, err := randomName()
	if err != nil {
		t.Fatalf("testpg: gerando nome: %v", err)
	}
	db := &Database{Name: name, Config: cfg}
	if err := db.exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("testpg: criando banco %s: %v", name, err)
	}

	pc, err := cfg.PoolConfig(name)
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	db.Pool, err = pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatalf("testpg: abrindo pool: %v", err)
	}

	t.Cleanup(func() {
		// O contexto do teste já foi cancelado quando o Cleanup roda: usa um próprio.
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		db.Pool.Close()
		if err := db.Drop(cctx); err != nil {
			t.Errorf("testpg: removendo banco %s: %v", name, err)
		}
	})
	return db
}

// EnsureRole garante que o papel de login exista na instância de teste. Papéis são do
// cluster, compartilhados por todos os bancos de teste: se outro pacote, em paralelo, criar o
// mesmo papel primeiro, a corrida é absorvida e o papel existente é aproveitado. Por isso a
// senha precisa ser a mesma em todos os testes que usam o papel.
func (d *Database) EnsureRole(t testing.TB, name, password string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// DO não aceita parâmetros: nome e senha entram já escapados (identificador e literal).
	sql := fmt.Sprintf(`DO $$ BEGIN
  CREATE ROLE %s LOGIN PASSWORD %s;
EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
END $$`, pgx.Identifier{name}.Sanitize(), quoteLiteral(password))
	if err := d.exec(ctx, sql); err != nil {
		t.Fatalf("testpg: criando papel %s: %v", name, err)
	}
}

// ConnectAs abre um pool neste banco com outro papel (por exemplo, o papel restrito do
// serviço), fechado ao fim do teste.
func (d *Database) ConnectAs(t testing.TB, user, password string) *pgxpool.Pool {
	t.Helper()
	cfg := d.Config
	cfg.User, cfg.Password = user, password
	pc, err := cfg.PoolConfig(d.Name)
	if err != nil {
		t.Fatalf("testpg: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("testpg: abrindo pool como %s: %v", user, err)
	}
	// Registrado depois do Cleanup de New, roda antes dele: fecha o pool antes do DROP.
	t.Cleanup(pool.Close)
	return pool
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Drop remove o banco, derrubando conexões abertas. É idempotente.
func (d *Database) Drop(ctx context.Context) error {
	return d.exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{d.Name}.Sanitize()+" WITH (FORCE)")
}

// exec roda um comando no banco administrativo (CREATE e DROP DATABASE não rodam no próprio banco).
func (d *Database) exec(ctx context.Context, sql string) error {
	pc, err := d.Config.PoolConfig(adminDatabase)
	if err != nil {
		return err
	}
	conn, err := pgx.ConnectConfig(ctx, pc.ConnConfig)
	if err != nil {
		return fmt.Errorf("conectando ao Postgres de teste: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, sql)
	return err
}

func randomName() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "t_" + hex.EncodeToString(b), nil
}
