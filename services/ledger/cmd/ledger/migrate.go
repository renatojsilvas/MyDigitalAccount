package main

import (
	"context"
	"io"

	"conta.local/ledger/internal/platform/config"
	"conta.local/ledger/internal/platform/logging"
	"conta.local/ledger/internal/platform/postgres"
	"conta.local/ledger/migrations"
)

// runMigrate aplica as migrations e sai: é o job que roda antes do serviço, com o papel
// dono do schema (ledger_owner). O serviço em si roda com ledger_app e não tem DDL.
//
// Não exporta telemetria por OTLP: é um processo de segundos, sem requisições; o log
// estruturado no stdout fica no histórico do contêiner (e no Loki, pelo coletor de logs
// do orquestrador, quando houver).
func runMigrate(ctx context.Context, lookup config.Lookup, stdout io.Writer) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	logger := logging.New(stdout, cfg.LogLevel, nil)

	applied, err := postgres.Migrate(ctx, cfg.Postgres, migrations.FS)
	if err != nil {
		return err
	}
	for _, a := range applied {
		logger.InfoContext(ctx, "migration aplicada",
			"version", a.Version, "file", a.Name, "duration_ms", a.Duration.Milliseconds())
	}
	logger.InfoContext(ctx, "migrations em dia",
		"applied", len(applied), "postgres_db", cfg.Postgres.Database, "postgres_user", cfg.Postgres.User)
	return nil
}
