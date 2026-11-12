// Command transfers é o composition root do serviço que orquestra o Pix (ADR-0007): as
// dependências são criadas e ligadas à mão, sem framework de injeção e sem estado global.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	antifraudev1 "conta.local/gen/go/conta/antifraude/v1"
	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/platform/admin"
	"conta.local/platform/grpcclient"
	"conta.local/platform/grpcserver"
	"conta.local/platform/healthcheck"
	"conta.local/platform/logging"
	"conta.local/platform/pg"
	"conta.local/platform/telemetry"
	"conta.local/transfers/internal/clients"
	"conta.local/transfers/internal/grpcapi"
	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/pixotel"
	"conta.local/transfers/internal/platform/config"
	transferspg "conta.local/transfers/internal/postgres"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
	"conta.local/transfers/internal/sagaotel"
	"conta.local/transfers/migrations"
)

const serviceName = "transfers"

// version é preenchida no build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, runFn := serviceName, func() error { return run(ctx, os.LookupEnv, os.Stdout) }
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			cmd, runFn = "transfers healthcheck", func() error { return runHealthcheck(ctx, os.LookupEnv) }
		case "migrate":
			cmd, runFn = "transfers migrate", func() error { return runMigrate(ctx, os.LookupEnv, os.Stdout) }
		default:
			fmt.Fprintf(os.Stderr, "transfers: subcomando desconhecido %q (use healthcheck ou migrate)\n", os.Args[1])
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

// runMigrate é o job que roda antes do serviço, com o papel dono do schema (transfers_owner).
func runMigrate(ctx context.Context, lookup config.Lookup, stdout io.Writer) error {
	cfg, err := config.LoadDatabase(lookup)
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

	cal, err := pix.NewCalendar()
	if err != nil {
		return err
	}
	observer, err := pixotel.New(tel.Meter)
	if err != nil {
		return fmt.Errorf("métricas de negócio: %w", err)
	}
	store := transferspg.NewStore(pool)

	// Clientes das dependências: cada um com o seu circuito e a sua política de retry.
	telSaga, err := sagaotel.New(tel.Tracer, tel.Meter, tel.Propagator, "antifraude", "ledger", "spi")
	if err != nil {
		return fmt.Errorf("métricas da saga: %w", err)
	}
	connOpts := grpcclient.Options{TracerProvider: tel.Tracer, MeterProvider: tel.Meter, Propagator: tel.Propagator}
	conns := map[string]*grpc.ClientConn{}
	for name, addr := range map[string]string{
		"antifraude": cfg.Dependencies.AntifraudeAddr, "ledger": cfg.Dependencies.LedgerAddr, "spi": cfg.Dependencies.SPIAddr,
	} {
		conn, err := grpcclient.Dial(addr, connOpts)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
		conns[name] = conn
	}
	// Aquecimento: abre as conexões antes de aceitar Pix, para o primeiro não pagar a partida a
	// frio (sem isso, o primeiro Pix depois de um reinício estourava o prazo do antifraude e era
	// recusado). Em paralelo e nunca derruba o boot: dependência fora do ar só gera aviso.
	var warm errgroup.Group
	for name, conn := range conns {
		warm.Go(func() error {
			if !grpcclient.WarmUp(ctx, conn, cfg.Dependencies.WarmUpTimeout) {
				logger.WarnContext(ctx, "aquecimento da conexão falhou; a primeira chamada pagará a partida a frio",
					"dependency", name)
			}
			return nil
		})
	}
	_ = warm.Wait()
	breaker := resilience.DefaultBreaker()
	quick := func(timeout time.Duration) resilience.Policy {
		return resilience.Policy{Attempts: 3, Base: 50 * time.Millisecond, Max: 300 * time.Millisecond, Timeout: timeout}
	}
	runner := saga.NewRunner(saga.Config{
		Owner: hostname, LeaseTTL: cfg.Saga.LeaseTTL, Workers: cfg.Saga.Workers, PollInterval: cfg.Saga.PollInterval,
		RetryDelay: cfg.Saga.RetryDelay, DailyLimitCents: cfg.DailyLimitCents, FanOutTimeout: cfg.Saga.FanOutTimeout,
		SettleAttempts: cfg.Saga.SettleAttempts,
		SettleBackoff: resilience.Policy{
			Base: cfg.Saga.SettleBackoffBase, Max: cfg.Saga.SettleBackoffMax, Timeout: cfg.Saga.SettleTimeout,
		},
		ConfirmAfter:    cfg.Saga.ConfirmAfter,
		ConfirmInterval: resilience.Policy{Base: cfg.Saga.ConfirmAfter, Max: cfg.Saga.ConfirmIntervalMax},
		ConfirmDeadline: cfg.Saga.ConfirmDeadline, ConfirmWorkers: cfg.Saga.ConfirmWorkers,
	}, saga.Deps{
		Store: store,
		Antifraud: clients.NewAntifraude(antifraudev1.NewAntifraudeServiceClient(conns["antifraude"]),
			clients.NewDependency("antifraude", breaker, quick(cfg.Dependencies.AntifraudeTimeout), telSaga)),
		Ledger: clients.NewLedger(ledgerv1.NewLedgerServiceClient(conns["ledger"]),
			clients.NewDependency("ledger", breaker, quick(cfg.Dependencies.LedgerTimeout), telSaga)),
		SPI: clients.NewSPI(spiv1.NewSpiServiceClient(conns["spi"]),
			clients.NewDependency("spi", breaker, resilience.Policy{Attempts: 1}, telSaga)),
		Tracing: telSaga, Observer: telSaga, Logger: logger,
	})
	if err := telSaga.RegisterPending(tel.Meter, store.PendingStats); err != nil {
		return fmt.Errorf("métricas de pendentes: %w", err)
	}
	svc := pix.NewService(store, cal, cfg.RequestHashKey, cfg.DailyLimitCents,
		pix.WithObserver(observer), pix.WithNotify(runner.Notify))

	// O contexto de trace do aceite é gravado na transferência (ADR-0007, seção 4).
	carrier := func(ctx context.Context) map[string]string {
		c := propagation.MapCarrier{}
		tel.Propagator.Inject(ctx, c)
		return c
	}

	healthSrv := health.NewServer()
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger: logger, TracerProvider: tel.Tracer, MeterProvider: tel.Meter, Propagator: tel.Propagator,
		Interceptors: []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
	})
	grpcapi.Register(srv, grpcapi.NewHandler(svc, carrier))
	grpcserver.RegisterPing(srv, grpcserver.NewPingHandler(serviceName))
	healthpb.RegisterHealthServer(srv, healthSrv)

	// O /readyz depende do Postgres: sem o banco não há aceite nem consulta.
	adminSrv := admin.New(logger, admin.Check{Name: "postgres", Fn: func(ctx context.Context) error { return pg.Ping(ctx, pool) }})

	logger.InfoContext(ctx, "serviço iniciando",
		"version", version, "environment", cfg.Environment,
		"grpc_addr", grpcLis.Addr().String(), "admin_addr", adminLis.Addr().String(),
		"postgres_host", cfg.Postgres.Host, "postgres_db", cfg.Postgres.Database,
		"daily_limit_cents", cfg.DailyLimitCents, "runner_workers", cfg.Saga.Workers,
		"antifraude", cfg.Dependencies.AntifraudeAddr, "ledger", cfg.Dependencies.LedgerAddr, "spi", cfg.Dependencies.SPIAddr,
		"otlp_export", cfg.OTLPEndpoint != "")

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcserver.Serve(gctx, srv, grpcLis, cfg.ShutdownTimeout) })
	g.Go(func() error { return adminSrv.Serve(gctx, adminLis, cfg.ShutdownTimeout) })
	// O runner da saga: para de pegar trabalho no sinal e espera as etapas em andamento.
	g.Go(func() error { return runner.Run(gctx) })
	// O worker de confirmação dos Pix pendentes (seção 4.5): pool próprio, lê do banco.
	g.Go(func() error { return runner.RunConfirmations(gctx) })
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
		fmt.Fprintln(os.Stderr, "transfers: descarregando telemetria:", err)
	}
	return runErr
}
