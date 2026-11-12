package pg

import (
	"context"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Applied descreve uma migration aplicada nesta execução.
type Applied struct {
	Version  int64
	Name     string
	Duration time.Duration
}

// Migrate aplica as migrations pendentes de fsys e devolve as que aplicou (nenhuma, se o
// banco já está em dia). Deve rodar com o papel dono do schema, nunca com o do serviço.
//
// O session locker usa um advisory lock do Postgres: se duas réplicas do job rodarem ao mesmo
// tempo, a segunda espera a primeira terminar e então não encontra nada pendente.
func Migrate(ctx context.Context, cfg Config, fsys fs.FS) ([]Applied, error) {
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*pc.ConnConfig)
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("criando o lock das migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, fsys, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("carregando as migrations: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return nil, fmt.Errorf("aplicando as migrations: %w", err)
	}
	applied := make([]Applied, 0, len(results))
	for _, r := range results {
		applied = append(applied, Applied{Version: r.Source.Version, Name: r.Source.Path, Duration: r.Duration})
	}
	return applied, nil
}
