package config_test

import (
	"strings"
	"testing"
	"time"

	"conta.local/spi-fake/internal/platform/config"
	"conta.local/spi-fake/internal/settlement"
)

func env(m map[string]string) config.Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"SPI_DB_USER": "u", "SPI_DB_PASSWORD": "p"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50071" || cfg.AdminAddr != ":8083" || cfg.Postgres.Database != "spi" ||
		cfg.Behavior != (settlement.Behavior{Mode: settlement.ModeSettle}) || cfg.MaxHold != 30*time.Second {
		t.Fatalf("padrões = %+v", cfg)
	}
}

func TestBehaviorFromEnv(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"SPI_DB_USER": "u", "SPI_DB_PASSWORD": "p",
		"SPI_SETTLE_MODE": "no_settle_no_response", "SPI_FAIL_STATUS_QUERIES": "true", "SPI_LATENCY": "200ms",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := settlement.Behavior{Mode: settlement.ModeNoSettleNoResponse, FailStatusQueries: true, Latency: 200 * time.Millisecond}
	if cfg.Behavior != want {
		t.Fatalf("comportamento = %+v, want %+v", cfg.Behavior, want)
	}
}

func TestInvalidListsEveryError(t *testing.T) {
	_, err := config.Load(env(map[string]string{
		"SPI_SETTLE_MODE": "liquidar", "SPI_FAIL_STATUS_QUERIES": "talvez", "SPI_LATENCY": "-1s",
	}))
	if err == nil {
		t.Fatal("configuração inválida aceita")
	}
	for _, want := range []string{"SPI_SETTLE_MODE", "SPI_FAIL_STATUS_QUERIES", "SPI_DB_USER", "SPI_DB_PASSWORD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("erro não cita %s: %v", want, err)
		}
	}
}
