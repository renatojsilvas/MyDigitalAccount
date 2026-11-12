// Package config carrega e valida a configuração do antifraude a partir de variáveis de
// ambiente. A validação roda no boot: configuração inválida impede o serviço de subir.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"conta.local/antifraude/internal/faults"
	"conta.local/antifraude/internal/risk"
)

// Config é a configuração já validada.
type Config struct {
	Environment     string
	LogLevel        slog.Level
	GRPCAddr        string
	AdminAddr       string
	OTLPEndpoint    string // vazio desliga a exportação OTLP
	ShutdownTimeout time.Duration

	RedisAddr string
	// RedisTimeout limita cada avaliação no Redis; estourado, a decisão sai degradada.
	RedisTimeout time.Duration
	Rules        risk.Rules
	Faults       faults.Config
}

// Lookup tem a assinatura de os.LookupEnv; receber a função deixa os testes sem estado global.
type Lookup func(key string) (string, bool)

var environments = map[string]bool{"development": true, "test": true, "production": true}

// reader lê variáveis com padrão e acumula os erros, para o boot listar todos de uma vez.
type reader struct {
	lookup Lookup
	errs   []error
}

func (r *reader) str(key, def string) string {
	if v, ok := r.lookup(key); ok && v != "" {
		return v
	}
	return def
}

func (r *reader) duration(key, def string) time.Duration {
	d, err := time.ParseDuration(r.str(key, def))
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s inválido: %w", key, err))
	}
	return d
}

func (r *reader) int64(key string, def int64) int64 {
	n, err := strconv.ParseInt(r.str(key, strconv.FormatInt(def, 10)), 10, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s inválido: %w", key, err))
	}
	return n
}

// Load lê as variáveis, aplica os padrões e devolve todos os erros de uma vez.
func Load(lookup Lookup) (Config, error) {
	r := &reader{lookup: lookup}
	def := risk.DefaultRules()
	cfg := Config{
		Environment:     r.str("ENVIRONMENT", "development"),
		GRPCAddr:        r.str("ANTIFRAUDE_GRPC_ADDR", ":50061"),
		AdminAddr:       r.str("ANTIFRAUDE_ADMIN_ADDR", ":8082"),
		OTLPEndpoint:    r.str("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT", "10s"),
		RedisAddr:       r.str("REDIS_ADDR", "redis:6379"),
		RedisTimeout:    r.duration("ANTIFRAUDE_REDIS_TIMEOUT", "50ms"),
		Rules: risk.Rules{
			HighAmountCents:     r.int64("ANTIFRAUDE_HIGH_AMOUNT_CENTS", def.HighAmountCents),
			VelocityCountMedium: r.int64("ANTIFRAUDE_VELOCITY_COUNT_MEDIUM", def.VelocityCountMedium),
			VelocityCountHigh:   r.int64("ANTIFRAUDE_VELOCITY_COUNT_HIGH", def.VelocityCountHigh),
			VelocitySumMedium:   r.int64("ANTIFRAUDE_VELOCITY_SUM_MEDIUM_CENTS", def.VelocitySumMedium),
			VelocitySumHigh:     r.int64("ANTIFRAUDE_VELOCITY_SUM_HIGH_CENTS", def.VelocitySumHigh),
			DenyScore:           int(r.int64("ANTIFRAUDE_DENY_SCORE", int64(def.DenyScore))),
			DegradedMaxAmount:   r.int64("ANTIFRAUDE_DEGRADED_MAX_AMOUNT_CENTS", def.DegradedMaxAmount),
		},
		Faults: faults.Config{Latency: r.duration("ANTIFRAUDE_FAULT_LATENCY", "0s")},
	}
	if rate, err := strconv.ParseFloat(r.str("ANTIFRAUDE_FAULT_ERROR_RATE", "0"), 64); err != nil {
		r.errs = append(r.errs, fmt.Errorf("ANTIFRAUDE_FAULT_ERROR_RATE inválido: %w", err))
	} else {
		cfg.Faults.ErrorRate = rate
	}

	if !environments[cfg.Environment] {
		r.errs = append(r.errs, fmt.Errorf("ENVIRONMENT %q inválido: use development, test ou production", cfg.Environment))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(r.str("LOG_LEVEL", "info"))); err != nil {
		r.errs = append(r.errs, fmt.Errorf("LOG_LEVEL inválido: %w", err))
	}
	if cfg.ShutdownTimeout <= 0 {
		r.errs = append(r.errs, errors.New("SHUTDOWN_TIMEOUT deve ser positivo"))
	}
	if cfg.RedisTimeout < time.Millisecond || cfg.RedisTimeout > time.Second {
		r.errs = append(r.errs, errors.New("ANTIFRAUDE_REDIS_TIMEOUT deve estar entre 1ms e 1s (prazo curto, seção 4.3)"))
	}
	if err := cfg.Rules.Validate(); err != nil {
		r.errs = append(r.errs, fmt.Errorf("regras do antifraude: %w", err))
	}
	if err := cfg.Faults.Validate(); err != nil {
		r.errs = append(r.errs, fmt.Errorf("ANTIFRAUDE_FAULT_*: %w", err))
	}
	if len(r.errs) > 0 {
		return Config{}, fmt.Errorf("configuração inválida: %w", errors.Join(r.errs...))
	}
	return cfg, nil
}
