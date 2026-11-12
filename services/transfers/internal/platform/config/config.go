// Package config carrega e valida a configuração do transfers a partir de variáveis de
// ambiente. A validação roda no boot: configuração inválida impede o serviço de subir.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"conta.local/platform/pg"
)

// minHashKeyBytes é o tamanho mínimo do segredo do HMAC: o mesmo do bloco do SHA-256 seria
// exagero, 32 bytes (256 bits) é o que a chave do HMAC-SHA256 precisa para não ser o elo fraco.
const minHashKeyBytes = 32

// Config é a configuração já validada do serviço.
type Config struct {
	Database
	Environment     string
	GRPCAddr        string
	AdminAddr       string
	OTLPEndpoint    string // vazio desliga a exportação OTLP
	ShutdownTimeout time.Duration
	// DailyLimitCents é o limite diário de Pix por conta (ADR-0007, seção 6).
	DailyLimitCents int64
	// RequestHashKey assina o pedido na tabela de idempotência (HMAC-SHA256). Segredo.
	RequestHashKey []byte
}

// Database é o que o job de migration precisa: o banco e o nível de log.
type Database struct {
	LogLevel slog.Level
	Postgres pg.Config
}

// Lookup tem a assinatura de os.LookupEnv; receber a função deixa os testes sem estado global.
type Lookup func(key string) (string, bool)

var environments = map[string]bool{"development": true, "test": true, "production": true}

type reader struct {
	lookup Lookup
	errs   []error
}

func (r *reader) get(key, def string) string {
	if v, ok := r.lookup(key); ok && v != "" {
		return v
	}
	return def
}

func (r *reader) fail(format string, args ...any) {
	r.errs = append(r.errs, fmt.Errorf(format, args...))
}

func (r *reader) err() error {
	if len(r.errs) == 0 {
		return nil
	}
	return fmt.Errorf("configuração inválida: %w", errors.Join(r.errs...))
}

func (r *reader) database() Database {
	var d Database
	if err := d.LogLevel.UnmarshalText([]byte(r.get("LOG_LEVEL", "info"))); err != nil {
		r.fail("LOG_LEVEL inválido: %w", err)
	}
	d.Postgres = pg.Config{
		Host: r.get("POSTGRES_HOST", "postgres"), User: r.get("TRANSFERS_DB_USER", ""),
		Password: r.get("TRANSFERS_DB_PASSWORD", ""), Database: r.get("TRANSFERS_DB_NAME", "transfers"),
	}
	port, err := strconv.ParseUint(r.get("POSTGRES_PORT", "5432"), 10, 16)
	if err != nil {
		r.fail("POSTGRES_PORT inválida: %w", err)
	}
	d.Postgres.Port = uint16(port)
	maxConns, err := strconv.ParseInt(r.get("TRANSFERS_DB_MAX_CONNS", "10"), 10, 32)
	if err != nil || maxConns < 1 || maxConns > 200 {
		r.fail("TRANSFERS_DB_MAX_CONNS deve ser um inteiro entre 1 e 200")
	}
	d.Postgres.MaxConns = int32(maxConns)
	if d.Postgres.User == "" {
		r.fail("TRANSFERS_DB_USER é obrigatória")
	}
	if d.Postgres.Password == "" {
		r.fail("TRANSFERS_DB_PASSWORD é obrigatória")
	}
	return d
}

// LoadDatabase carrega só o necessário para o job de migration.
func LoadDatabase(lookup Lookup) (Database, error) {
	r := &reader{lookup: lookup}
	d := r.database()
	return d, r.err()
}

// Load carrega a configuração do serviço e devolve todos os erros de uma vez.
func Load(lookup Lookup) (Config, error) {
	r := &reader{lookup: lookup}
	cfg := Config{
		Database:     r.database(),
		Environment:  r.get("ENVIRONMENT", "development"),
		GRPCAddr:     r.get("TRANSFERS_GRPC_ADDR", ":50081"),
		AdminAddr:    r.get("TRANSFERS_ADMIN_ADDR", ":8084"),
		OTLPEndpoint: r.get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
	if !environments[cfg.Environment] {
		r.fail("ENVIRONMENT %q inválido: use development, test ou production", cfg.Environment)
	}
	timeout, err := time.ParseDuration(r.get("SHUTDOWN_TIMEOUT", "10s"))
	if err != nil || timeout <= 0 {
		r.fail("SHUTDOWN_TIMEOUT deve ser uma duração positiva")
	}
	cfg.ShutdownTimeout = timeout
	limit, err := strconv.ParseInt(r.get("TRANSFERS_DAILY_LIMIT_CENTS", "200000"), 10, 64)
	if err != nil || limit <= 0 {
		r.fail("TRANSFERS_DAILY_LIMIT_CENTS deve ser um inteiro positivo (centavos)")
	}
	cfg.DailyLimitCents = limit
	cfg.RequestHashKey = []byte(r.get("TRANSFERS_REQUEST_HASH_KEY", ""))
	if len(cfg.RequestHashKey) < minHashKeyBytes {
		r.fail("TRANSFERS_REQUEST_HASH_KEY é obrigatória, com pelo menos %d bytes", minHashKeyBytes)
	}
	return cfg, r.err()
}
