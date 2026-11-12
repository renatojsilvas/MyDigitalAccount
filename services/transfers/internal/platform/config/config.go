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
	Dependencies   Dependencies
	Saga           Saga
}

// Dependencies são os endereços e prazos de cada chamada (ADR-0007, seção 7).
type Dependencies struct {
	AntifraudeAddr, LedgerAddr, SPIAddr string
	AntifraudeTimeout, LedgerTimeout    time.Duration
}

// Saga são os parâmetros do runner e do timeout na liquidação (ADR-0007, seções 4 e 5).
type Saga struct {
	Workers                             int
	PollInterval, LeaseTTL, RetryDelay  time.Duration
	FanOutTimeout                       time.Duration
	SettleTimeout                       time.Duration
	SettleAttempts                      int
	SettleBackoffBase, SettleBackoffMax time.Duration
	ConfirmAfter                        time.Duration
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

// duration lê uma duração positiva.
func (r *reader) duration(key, def string) time.Duration {
	d, err := time.ParseDuration(r.get(key, def))
	if err != nil || d <= 0 {
		r.fail("%s deve ser uma duração positiva", key)
	}
	return d
}

// count lê um inteiro entre lo e hi.
func (r *reader) count(key string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.get(key, strconv.Itoa(def)))
	if err != nil || n < lo || n > hi {
		r.fail("%s deve ser um inteiro entre %d e %d", key, lo, hi)
	}
	return n
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
	cfg.Dependencies = Dependencies{
		AntifraudeAddr:    r.get("TRANSFERS_ANTIFRAUDE_ADDR", "antifraude:50061"),
		LedgerAddr:        r.get("TRANSFERS_LEDGER_ADDR", "ledger:50051"),
		SPIAddr:           r.get("TRANSFERS_SPI_ADDR", "spi-fake:50071"),
		AntifraudeTimeout: r.duration("TRANSFERS_ANTIFRAUDE_TIMEOUT", "300ms"),
		LedgerTimeout:     r.duration("TRANSFERS_LEDGER_TIMEOUT", "500ms"),
	}
	cfg.Saga = Saga{
		Workers:           r.count("TRANSFERS_RUNNER_WORKERS", 8, 1, 256),
		PollInterval:      r.duration("TRANSFERS_RUNNER_POLL_INTERVAL", "500ms"),
		LeaseTTL:          r.duration("TRANSFERS_LEASE_TTL", "30s"),
		RetryDelay:        r.duration("TRANSFERS_RETRY_DELAY", "2s"),
		FanOutTimeout:     r.duration("TRANSFERS_FANOUT_TIMEOUT", "1s"),
		SettleTimeout:     r.duration("TRANSFERS_SPI_SETTLE_TIMEOUT", "1s"),
		SettleAttempts:    r.count("TRANSFERS_SPI_SETTLE_ATTEMPTS", 3, 1, 10),
		SettleBackoffBase: r.duration("TRANSFERS_SPI_SETTLE_BACKOFF_BASE", "200ms"),
		SettleBackoffMax:  r.duration("TRANSFERS_SPI_SETTLE_BACKOFF_MAX", "1s"),
		ConfirmAfter:      r.duration("TRANSFERS_CONFIRM_INTERVAL_BASE", "2s"),
	}
	// O lease precisa cobrir a etapa mais longa (as tentativas de liquidação), senão outro
	// runner a tomaria no meio: o fencing token barraria a escrita, mas o trabalho se repetiria.
	worst := time.Duration(cfg.Saga.SettleAttempts) * (cfg.Saga.SettleTimeout + cfg.Saga.SettleBackoffMax)
	if cfg.Saga.LeaseTTL <= 2*worst {
		r.fail("TRANSFERS_LEASE_TTL (%v) deve passar do dobro do pior caso da liquidação (%v)", cfg.Saga.LeaseTTL, worst)
	}
	cfg.RequestHashKey = []byte(r.get("TRANSFERS_REQUEST_HASH_KEY", ""))
	if len(cfg.RequestHashKey) < minHashKeyBytes {
		r.fail("TRANSFERS_REQUEST_HASH_KEY é obrigatória, com pelo menos %d bytes", minHashKeyBytes)
	}
	return cfg, r.err()
}
