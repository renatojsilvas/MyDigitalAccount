// Package config carrega e valida a configuração do serviço a partir de variáveis de
// ambiente. A validação roda no boot: configuração inválida impede o serviço de subir.
package config

import (
	"errors"
	"fmt"
	"log/slog"
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

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %w", errors.Join(errs...))
	}
	return cfg, nil
}
