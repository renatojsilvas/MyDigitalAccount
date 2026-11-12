package config_test

import (
	"strings"
	"testing"

	"conta.local/transfers/internal/platform/config"
)

func env(m map[string]string) config.Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func valid() map[string]string {
	return map[string]string{
		"TRANSFERS_DB_USER": "u", "TRANSFERS_DB_PASSWORD": "p",
		"TRANSFERS_REQUEST_HASH_KEY": strings.Repeat("s", 32),
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := config.Load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":50081" || cfg.AdminAddr != ":8084" || cfg.DailyLimitCents != 200_000 ||
		cfg.Postgres.Database != "transfers" || len(cfg.RequestHashKey) != 32 {
		t.Fatalf("padrões = %+v", cfg)
	}
}

func TestHashKeyIsRequiredOnlyForTheService(t *testing.T) {
	m := valid()
	m["TRANSFERS_REQUEST_HASH_KEY"] = "curta"
	if _, err := config.Load(env(m)); err == nil || !strings.Contains(err.Error(), "TRANSFERS_REQUEST_HASH_KEY") {
		t.Fatalf("segredo curto aceito: %v", err)
	}
	// O job de migration não precisa do segredo.
	if _, err := config.LoadDatabase(env(m)); err != nil {
		t.Fatalf("LoadDatabase: %v", err)
	}
}

func TestInvalidListsEveryError(t *testing.T) {
	_, err := config.Load(env(map[string]string{"TRANSFERS_DAILY_LIMIT_CENTS": "-1", "ENVIRONMENT": "x"}))
	for _, want := range []string{"TRANSFERS_DAILY_LIMIT_CENTS", "ENVIRONMENT", "TRANSFERS_DB_USER", "TRANSFERS_REQUEST_HASH_KEY"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("erro não cita %s: %v", want, err)
		}
	}
}

func TestSagaDefaultsFollowADR0007(t *testing.T) {
	cfg, err := config.Load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	s, d := cfg.Saga, cfg.Dependencies
	if s.SettleAttempts != 3 || s.SettleTimeout.String() != "1s" || s.SettleBackoffBase.String() != "200ms" ||
		s.SettleBackoffMax.String() != "1s" || s.ConfirmAfter.String() != "2s" || s.LeaseTTL.String() != "30s" ||
		d.AntifraudeTimeout.String() != "300ms" || d.LedgerTimeout.String() != "500ms" || s.FanOutTimeout.String() != "1s" ||
		s.ConfirmIntervalMax.String() != "30s" || s.ConfirmDeadline.String() != "10m0s" {
		t.Fatalf("saga = %+v, dependências = %+v", s, d)
	}
}

func TestLeaseMustCoverTheSettlementStep(t *testing.T) {
	m := valid()
	m["TRANSFERS_LEASE_TTL"] = "5s" // 3 x (1s + 1s) = 6s; o lease precisa passar de 12s
	if _, err := config.Load(env(m)); err == nil || !strings.Contains(err.Error(), "TRANSFERS_LEASE_TTL") {
		t.Fatalf("lease curto aceito: %v", err)
	}
}
