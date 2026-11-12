// Package config carrega e valida a configuração do spi-fake a partir de variáveis de ambiente.
// A validação roda no boot: configuração inválida impede o serviço de subir.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"conta.local/platform/pg"
	"conta.local/spi-fake/internal/settlement"
)

// Config é a configuração já validada.
type Config struct {
	Environment     string
	LogLevel        slog.Level
	GRPCAddr        string
	AdminAddr       string
	OTLPEndpoint    string // vazio desliga a exportação OTLP
	ShutdownTimeout time.Duration
	Postgres        pg.Config
	Behavior        settlement.Behavior
	// MaxHold limita quanto tempo os modos "sem responder" seguram uma chamada sem prazo.
	MaxHold time.Duration
}

// Lookup tem a assinatura de os.LookupEnv; receber a função deixa os testes sem estado global.
type Lookup func(key string) (string, bool)

var environments = map[string]bool{"development": true, "test": true, "production": true}

// Load lê as variáveis, aplica os padrões e devolve todos os erros de uma vez.
func Load(lookup Lookup) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}
	var errs []error
	duration := func(key, def string) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s inválido: %w", key, err))
		}
		return d
	}

	cfg := Config{
		Environment:     get("ENVIRONMENT", "development"),
		GRPCAddr:        get("SPI_GRPC_ADDR", ":50071"),
		AdminAddr:       get("SPI_ADMIN_ADDR", ":8083"),
		OTLPEndpoint:    get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ShutdownTimeout: duration("SHUTDOWN_TIMEOUT", "10s"),
		MaxHold:         duration("SPI_MAX_HOLD", "30s"),
		Postgres: pg.Config{
			Host: get("POSTGRES_HOST", "postgres"), User: get("SPI_DB_USER", ""),
			Password: get("SPI_DB_PASSWORD", ""), Database: get("SPI_DB_NAME", "spi"),
		},
	}
	if !environments[cfg.Environment] {
		errs = append(errs, fmt.Errorf("ENVIRONMENT %q inválido: use development, test ou production", cfg.Environment))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL inválido: %w", err))
	}
	if cfg.ShutdownTimeout <= 0 || cfg.MaxHold <= 0 {
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT e SPI_MAX_HOLD devem ser positivos"))
	}

	mode, err := settlement.ParseMode(get("SPI_SETTLE_MODE", "settle"))
	if err != nil {
		errs = append(errs, fmt.Errorf("SPI_SETTLE_MODE: %w", err))
	}
	failQueries, err := strconv.ParseBool(get("SPI_FAIL_STATUS_QUERIES", "false"))
	if err != nil {
		errs = append(errs, fmt.Errorf("SPI_FAIL_STATUS_QUERIES inválido: %w", err))
	}
	cfg.Behavior = settlement.Behavior{Mode: mode, FailStatusQueries: failQueries, Latency: duration("SPI_LATENCY", "0s")}
	if mode != 0 {
		if err := cfg.Behavior.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("comportamento do simulador: %w", err))
		}
	}

	port, err := strconv.ParseUint(get("POSTGRES_PORT", "5432"), 10, 16)
	if err != nil {
		errs = append(errs, fmt.Errorf("POSTGRES_PORT inválida: %w", err))
	}
	cfg.Postgres.Port = uint16(port)
	maxConns, err := strconv.ParseInt(get("SPI_DB_MAX_CONNS", "10"), 10, 32)
	if err != nil || maxConns < 1 || maxConns > 200 {
		errs = append(errs, errors.New("SPI_DB_MAX_CONNS deve ser um inteiro entre 1 e 200"))
	}
	cfg.Postgres.MaxConns = int32(maxConns)
	if cfg.Postgres.User == "" {
		errs = append(errs, errors.New("SPI_DB_USER é obrigatória"))
	}
	if cfg.Postgres.Password == "" {
		errs = append(errs, errors.New("SPI_DB_PASSWORD é obrigatória"))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %w", errors.Join(errs...))
	}
	return cfg, nil
}
