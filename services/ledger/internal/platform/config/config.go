// Package config carrega e valida a configuração do serviço a partir de variáveis de
// ambiente. A validação roda no boot: configuração inválida impede o serviço de subir.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Config é a configuração já validada do serviço.
type Config struct {
	Environment     string
	LogLevel        slog.Level
	GRPCAddr        string
	AdminAddr       string
	OTLPEndpoint    string // vazio desliga a exportação OTLP; os logs seguem no stdout
	ShutdownTimeout time.Duration
	Postgres        Postgres
}

// Postgres é o endereço e as credenciais do banco do ledger (um banco por serviço).
// A senha nunca deve ir para log. O serviço usa o papel ledger_app (só DML); o subcomando
// `ledger migrate` recebe, pelas mesmas variáveis, o papel ledger_owner (ADR-0004).
type Postgres struct {
	Host     string
	Port     uint16
	User     string
	Password string
	Database string
	// LockTimeout limita a espera por um lock de linha (a conta travada por outro débito).
	// Estourado, o Postgres cancela o comando e o cliente pode repetir com a mesma chave.
	LockTimeout time.Duration
	// MaxConns é o tamanho máximo do pool. Explícito para a utilização do pool (conexões em uso
	// / máximo, o U do método USE) ter um denominador conhecido.
	MaxConns int32
}

// Lookup tem a assinatura de os.LookupEnv. Receber a função, em vez de ler o ambiente
// direto, deixa os testes sem estado global.
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
	cfg := Config{
		Environment:  get("ENVIRONMENT", "development"),
		GRPCAddr:     get("LEDGER_GRPC_ADDR", ":50051"),
		AdminAddr:    get("LEDGER_ADMIN_ADDR", ":8081"),
		OTLPEndpoint: get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}

	if !environments[cfg.Environment] {
		errs = append(errs, fmt.Errorf("ENVIRONMENT %q inválido: use development, test ou production", cfg.Environment))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL inválido: %w", err))
	}
	timeout, err := time.ParseDuration(get("SHUTDOWN_TIMEOUT", "10s"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT inválido: %w", err))
	case timeout <= 0:
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT deve ser positivo"))
	}
	cfg.ShutdownTimeout = timeout

	pg, pgErrs := loadPostgres(get)
	cfg.Postgres = pg
	errs = append(errs, pgErrs...)

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func loadPostgres(get func(key, def string) string) (Postgres, []error) {
	var errs []error
	pg := Postgres{
		Host:     get("POSTGRES_HOST", "postgres"),
		User:     get("LEDGER_DB_USER", ""),
		Password: get("LEDGER_DB_PASSWORD", ""),
		Database: get("LEDGER_DB_NAME", "ledger"),
	}
	port, err := strconv.ParseUint(get("POSTGRES_PORT", "5432"), 10, 16)
	if err != nil {
		errs = append(errs, fmt.Errorf("POSTGRES_PORT inválida: %w", err))
	}
	pg.Port = uint16(port)
	lockTimeout, err := time.ParseDuration(get("LEDGER_DB_LOCK_TIMEOUT", "1s"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("LEDGER_DB_LOCK_TIMEOUT inválido: %w", err))
	case lockTimeout < time.Millisecond:
		// Zero no Postgres desliga o limite: espera infinita, justamente o que se quer evitar.
		errs = append(errs, errors.New("LEDGER_DB_LOCK_TIMEOUT deve ser de pelo menos 1ms"))
	}
	pg.LockTimeout = lockTimeout
	maxConns, err := strconv.ParseInt(get("LEDGER_DB_MAX_CONNS", "10"), 10, 32)
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("LEDGER_DB_MAX_CONNS inválido: %w", err))
	case maxConns < 1 || maxConns > 200:
		errs = append(errs, errors.New("LEDGER_DB_MAX_CONNS deve estar entre 1 e 200"))
	}
	pg.MaxConns = int32(maxConns)
	if pg.User == "" {
		errs = append(errs, errors.New("LEDGER_DB_USER é obrigatória"))
	}
	if pg.Password == "" {
		errs = append(errs, errors.New("LEDGER_DB_PASSWORD é obrigatória"))
	}
	return pg, errs
}
