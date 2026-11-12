package main

import (
	"context"

	"conta.local/ledger/internal/platform/config"
	"conta.local/platform/healthcheck"
)

// runHealthcheck é o subcomando `ledger healthcheck` do HEALTHCHECK da imagem final
// (distroless): carrega a configuração e consulta o /readyz da porta admin.
func runHealthcheck(ctx context.Context, lookup config.Lookup) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	return healthcheck.Probe(ctx, cfg.AdminAddr)
}
