// Package pg abre o pool de conexões de um serviço com o seu banco Postgres (um banco por
// serviço) e aplica as migrations. Comum aos serviços Go (ADR-0010).
package pg

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const connectTimeout = 5 * time.Second

// Config é o endereço e as credenciais do banco do serviço. A senha nunca deve ir para log.
type Config struct {
	Host     string
	Port     uint16
	User     string
	Password string
	Database string
	// LockTimeout limita a espera por um lock de linha; estourado, o Postgres cancela o
	// comando (erro repetível). Zero mantém o padrão do servidor.
	LockTimeout time.Duration
	// MaxConns é o tamanho máximo do pool; explícito para a utilização do pool (o U do método
	// USE) ter um denominador conhecido. Zero usa o padrão do pgx.
	MaxConns int32
}

// Option configura o pool aberto por Open.
type Option func(*options)

type options struct {
	tracer trace.TracerProvider
	meter  metric.MeterProvider
}

// WithTelemetry instrumenta o pool: um span por query (filho do span da chamada gRPC), a
// duração de cada operação e as métricas do pool (USE). Os providers entram explícitos, sem
// os globais do OpenTelemetry (ADR-0002).
func WithTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) Option {
	return func(o *options) { o.tracer, o.meter = tp, mp }
}

// Open abre o pool e confirma que o banco responde. Falha cedo: se o banco não está
// acessível no boot, o serviço não deve subir (o orquestrador o reinicia).
//
// Os campos da conexão são atribuídos um a um, em vez de montar uma URL, para a senha não
// precisar de escape e nunca aparecer numa string de conexão que possa ir parar em log.
func Open(ctx context.Context, cfg Config, opts ...Option) (*pgxpool.Pool, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	pc, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	if o.tracer != nil {
		pc.ConnConfig.Tracer = otelpgx.NewTracer(
			otelpgx.WithTracerProvider(o.tracer),
			otelpgx.WithMeterProvider(o.meter),
			// A espera por conexão aparece na métrica do pool (empty_acquire_wait_time); um
			// span por aquisição só dobraria o volume de spans. Os parâmetros das queries
			// (que podem conter CPF) ficam fora dos spans: é o padrão da biblioteca.
			otelpgx.WithDisableAcquireTracer(),
		)
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("criando pool: %w", err)
	}
	if o.meter != nil {
		if err := otelpgx.RecordStats(pool, otelpgx.WithStatsMeterProvider(o.meter)); err != nil {
			pool.Close()
			return nil, fmt.Errorf("métricas do pool: %w", err)
		}
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := Ping(pingCtx, pool); err != nil {
		pool.Close()
		// A mensagem do pgx não inclui a senha.
		return nil, fmt.Errorf("conectando a %s:%d/%s: %w", cfg.Host, cfg.Port, cfg.Database, err)
	}
	return pool, nil
}

// Ping confirma que o banco responde, no nível do protocolo: não passa pelo tracer de
// queries, então a sonda de prontidão (a cada poucos segundos) não gera traces órfãos.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	return conn.Conn().PgConn().Ping(ctx)
}

func poolConfig(cfg Config) (*pgxpool.Config, error) {
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
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.LockTimeout > 0 {
		// Parâmetro da sessão, enviado na conexão: vale para todo comando sem custar um SET
		// por transação.
		pc.ConnConfig.RuntimeParams["lock_timeout"] = strconv.FormatInt(cfg.LockTimeout.Milliseconds(), 10)
	}
	return pc, nil
}
