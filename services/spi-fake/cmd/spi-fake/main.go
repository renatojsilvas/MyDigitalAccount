// Command spi-fake é o simulador do SPI (seção 4.5 do plano, ADR-0007) e o composition root do
// serviço: as dependências são criadas e ligadas à mão, sem framework de injeção.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"conta.local/platform/admin"
	"conta.local/platform/grpcserver"
	"conta.local/platform/healthcheck"
	"conta.local/platform/logging"
	"conta.local/platform/pg"
	"conta.local/platform/telemetry"
	"conta.local/spi-fake/internal/grpcapi"
	"conta.local/spi-fake/internal/platform/config"
	spipg "conta.local/spi-fake/internal/postgres"
	"conta.local/spi-fake/internal/settlement"
	"conta.local/spi-fake/internal/settlementotel"
	"conta.local/spi-fake/migrations"
)

const serviceName = "spi-fake"

// version é preenchida no build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, runFn := serviceName, func() error { return run(ctx, os.LookupEnv, os.Stdout) }
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			cmd, runFn = "spi-fake healthcheck", func() error { return runHealthcheck(ctx, os.LookupEnv) }
		case "migrate":
			cmd, runFn = "spi-fake migrate", func() error { return runMigrate(ctx, os.LookupEnv, os.Stdout) }
		default:
			fmt.Fprintf(os.Stderr, "spi-fake: subcomando desconhecido %q (use healthcheck ou migrate)\n", os.Args[1])
			stop()
			os.Exit(2)
		}
	}
	if err := runFn(); err != nil {
		fmt.Fprintln(os.Stderr, cmd+":", err)
		stop()
		os.Exit(1)
	}
}

func runHealthcheck(ctx context.Context, lookup config.Lookup) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	return healthcheck.Probe(ctx, cfg.AdminAddr)
}

// runMigrate é o job que roda antes do serviço, com o papel dono do schema (spi_owner).
func runMigrate(ctx context.Context, lookup config.Lookup, stdout io.Writer) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	logger := logging.New(stdout, cfg.LogLevel, nil)
	applied, err := pg.Migrate(ctx, cfg.Postgres, migrations.FS)
	if err != nil {
		return err
	}
	for _, a := range applied {
		logger.InfoContext(ctx, "migration aplicada", "version", a.Version, "name", a.Name, "duration", a.Duration.String())
	}
	logger.InfoContext(ctx, "migrations em dia", "applied", len(applied),
		"postgres_db", cfg.Postgres.Database, "postgres_user", cfg.Postgres.User)
	return nil
}

func run(ctx context.Context, lookup config.Lookup, stdout io.Writer) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	tel, err := telemetry.New(ctx, telemetry.Config{
		ServiceName: serviceName, Version: version, Environment: cfg.Environment,
		InstanceID: hostname, OTLPEndpoint: cfg.OTLPEndpoint,
	})
	if err != nil {
		return err
	}
	logger := logging.New(stdout, cfg.LogLevel, tel.LogHandler(serviceName))

	var lc net.ListenConfig
	grpcLis, err := lc.Listen(ctx, "tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("ouvindo gRPC em %s: %w", cfg.GRPCAddr, err)
	}
	adminLis, err := lc.Listen(ctx, "tcp", cfg.AdminAddr)
	if err != nil {
		return fmt.Errorf("ouvindo admin em %s: %w", cfg.AdminAddr, err)
	}

	pool, err := pg.Open(ctx, cfg.Postgres, pg.WithTelemetry(tel.Tracer, tel.Meter))
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	sw := settlement.NewSwitch(cfg.Behavior)
	observer, err := settlementotel.New(tel.Meter, sw)
	if err != nil {
		return fmt.Errorf("métricas de negócio: %w", err)
	}
	svc := settlement.NewService(spipg.NewStore(pool), sw, cfg.MaxHold, settlement.WithObserver(observer))

	healthSrv := health.NewServer()
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger: logger, TracerProvider: tel.Tracer, MeterProvider: tel.Meter, Propagator: tel.Propagator,
		Interceptors: []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
	})
	grpcapi.Register(srv, grpcapi.NewHandler(svc, sw))
	grpcserver.RegisterPing(srv, grpcserver.NewPingHandler(serviceName))
	healthpb.RegisterHealthServer(srv, healthSrv)

	// O /readyz depende do Postgres: sem o banco o simulador não registra nem consulta.
	adminSrv := admin.New(logger, admin.Check{Name: "postgres", Fn: func(ctx context.Context) error { return pg.Ping(ctx, pool) }})

	b := cfg.Behavior
	logger.InfoContext(ctx, "serviço iniciando",
		"version", version, "environment", cfg.Environment,
		"grpc_addr", grpcLis.Addr().String(), "admin_addr", adminLis.Addr().String(),
		"postgres_host", cfg.Postgres.Host, "postgres_db", cfg.Postgres.Database,
		"settle_mode", b.Mode.String(), "fail_status_queries", b.FailStatusQueries, "latency", b.Latency.String(),
		"otlp_export", cfg.OTLPEndpoint != "")

	// O shutdownTimeout corta as chamadas seguradas pelos modos "sem responder" no desligamento.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcserver.Serve(gctx, srv, grpcLis, cfg.ShutdownTimeout) })
	g.Go(func() error { return adminSrv.Serve(gctx, adminLis, cfg.ShutdownTimeout) })
	g.Go(func() error {
		<-gctx.Done()
		adminSrv.SetReady(false)
		healthSrv.Shutdown()
		return nil
	})
	runErr := g.Wait()

	logger.InfoContext(context.WithoutCancel(ctx), "serviço encerrado", "error", runErr)
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := tel.Shutdown(flushCtx); err != nil {
		fmt.Fprintln(os.Stderr, "spi-fake: descarregando telemetria:", err)
	}
	return runErr
}
