// Command ledger é o composition root do serviço: aqui as dependências são criadas e
// ligadas à mão, sem framework de injeção e sem estado global.
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
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"conta.local/ledger/internal/grpcapi"
	"conta.local/ledger/internal/platform/admin"
	"conta.local/ledger/internal/platform/config"
	"conta.local/ledger/internal/platform/logging"
	"conta.local/ledger/internal/platform/postgres"
	"conta.local/ledger/internal/platform/telemetry"
)

const serviceName = "ledger"

// version é preenchida no build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	// Graceful shutdown: SIGINT ou SIGTERM (docker stop) cancelam o contexto raiz.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// `ledger healthcheck`: usado pelo HEALTHCHECK da imagem final, que não tem shell nem curl.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(ctx, os.LookupEnv); err != nil {
			fmt.Fprintln(os.Stderr, "ledger healthcheck:", err)
			stop()
			os.Exit(1)
		}
		return
	}

	if err := run(ctx, os.LookupEnv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ledger:", err)
		stop()
		os.Exit(1)
	}
}

// run monta e executa o serviço até ctx ser cancelado. Receber o ambiente e a saída
// por parâmetro permite testá-lo sem tocar em variáveis de ambiente do processo.
func run(ctx context.Context, lookup config.Lookup, stdout io.Writer) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}

	tel, err := telemetry.New(ctx, telemetry.Config{
		ServiceName: serviceName, Version: version, Environment: cfg.Environment, OTLPEndpoint: cfg.OTLPEndpoint,
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

	pool, err := postgres.Open(ctx, cfg.Postgres)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()

	healthSrv := health.NewServer() // grpc.health.v1, para clientes e balanceadores gRPC
	srv := grpcapi.NewServer(grpcapi.ServerOptions{
		Logger: logger, TracerProvider: tel.Tracer, MeterProvider: tel.Meter, Propagator: tel.Propagator,
	})
	grpcapi.RegisterPing(srv, grpcapi.NewPingHandler(serviceName))
	healthpb.RegisterHealthServer(srv, healthSrv)

	// O /readyz depende do Postgres: sem banco o ledger não pode atender (o /healthz não depende).
	adminSrv := admin.New(logger, admin.Check{Name: "postgres", Fn: pool.Ping})

	logger.InfoContext(ctx, "serviço iniciando",
		"version", version, "environment", cfg.Environment,
		"grpc_addr", grpcLis.Addr().String(), "admin_addr", adminLis.Addr().String(),
		"postgres_host", cfg.Postgres.Host, "postgres_db", cfg.Postgres.Database,
		"otlp_export", cfg.OTLPEndpoint != "")

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcapi.Serve(gctx, srv, grpcLis, cfg.ShutdownTimeout) })
	g.Go(func() error { return adminSrv.Serve(gctx, adminLis, cfg.ShutdownTimeout) })
	g.Go(func() error {
		// Ao receber o sinal, para de se anunciar como pronto antes de parar de servir.
		<-gctx.Done()
		adminSrv.SetReady(false)
		healthSrv.Shutdown()
		return nil
	})
	runErr := g.Wait()

	logger.InfoContext(context.WithoutCancel(ctx), "serviço encerrado", "error", runErr)
	// ctx já foi cancelado pelo sinal; descarregar a telemetria exige um contexto novo.
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := tel.Shutdown(flushCtx); err != nil {
		fmt.Fprintln(os.Stderr, "ledger: descarregando telemetria:", err)
	}
	return runErr
}
