// Package postgres abre o pool de conexões do ledger com o seu banco.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/ledger/internal/platform/config"
)

const connectTimeout = 5 * time.Second

// Open abre o pool e confirma que o banco responde. Falha cedo: se o banco não está
// acessível no boot, o serviço não deve subir (o orquestrador o reinicia).
//
// Os campos da conexão são atribuídos um a um, em vez de montar uma URL, para a senha não
// precisar de escape e nunca aparecer numa string de conexão que possa ir parar em log.
func Open(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("criando pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		// A mensagem do pgx não inclui a senha.
		return nil, fmt.Errorf("conectando a %s:%d/%s: %w", cfg.Host, cfg.Port, cfg.Database, err)
	}
	return pool, nil
}

func poolConfig(cfg config.Postgres) (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("configuração do pool: %w", err)
	}
	pc.ConnConfig.Host = cfg.Host
	pc.ConnConfig.Port = cfg.Port
	pc.ConnConfig.User = cfg.User
	pc.ConnConfig.Password = cfg.Password
	pc.ConnConfig.Database = cfg.Database
	pc.ConnConfig.ConnectTimeout = connectTimeout
	return pc, nil
}
