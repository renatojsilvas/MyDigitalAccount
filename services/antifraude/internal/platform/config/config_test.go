package config_test

import (
	"strings"
	"testing"
	"time"

	"conta.local/antifraude/internal/platform/config"
	"conta.local/antifraude/internal/risk"
)

func env(m map[string]string) config.Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50061" || cfg.AdminAddr != ":8082" || cfg.RedisAddr != "redis:6379" ||
		cfg.RedisTimeout != 50*time.Millisecond || cfg.Rules != risk.DefaultRules() || cfg.Faults.ErrorRate != 0 {
		t.Fatalf("padrões = %+v", cfg)
	}
}

func TestOverrides(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"ANTIFRAUDE_FAULT_LATENCY": "300ms", "ANTIFRAUDE_FAULT_ERROR_RATE": "0.25",
		"ANTIFRAUDE_DEGRADED_MAX_AMOUNT_CENTS": "10000", "ANTIFRAUDE_REDIS_TIMEOUT": "20ms",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Faults.Latency != 300*time.Millisecond || cfg.Faults.ErrorRate != 0.25 ||
		cfg.Rules.DegradedMaxAmount != 10_000 || cfg.RedisTimeout != 20*time.Millisecond {
		t.Fatalf("= %+v", cfg)
	}
}

func TestInvalidListsEveryError(t *testing.T) {
	_, err := config.Load(env(map[string]string{
		"ENVIRONMENT": "staging", "ANTIFRAUDE_FAULT_ERROR_RATE": "2", "ANTIFRAUDE_REDIS_TIMEOUT": "5s",
		"ANTIFRAUDE_DENY_SCORE": "abc", "LOG_LEVEL": "x",
	}))
	if err == nil {
		t.Fatal("configuração inválida aceita")
	}
	for _, want := range []string{"ENVIRONMENT", "ANTIFRAUDE_FAULT_", "ANTIFRAUDE_REDIS_TIMEOUT", "ANTIFRAUDE_DENY_SCORE", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("erro não cita %s: %v", want, err)
		}
	}
}
