// Command antifraude é o composition root do serviço (ADR-0008): as dependências são criadas e
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

	"github.com/redis/go-redis/extra/redisotel/v9"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"conta.local/antifraude/internal/faults"
	"conta.local/antifraude/internal/grpcapi"
	"conta.local/antifraude/internal/platform/config"
	"conta.local/antifraude/internal/redisvelocity"
	"conta.local/antifraude/internal/risk"
	"conta.local/antifraude/internal/riskotel"
	"conta.local/platform/admin"
	"conta.local/platform/grpcserver"
	"conta.local/platform/healthcheck"
	"conta.local/platform/logging"
	"conta.local/platform/telemetry"
)

const serviceName = "antifraude"

// version é preenchida no build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	// Graceful shutdown: SIGINT ou SIGTERM (docker stop) cancelam o contexto raiz.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, runFn := serviceName, func() error { return run(ctx, os.LookupEnv, os.Stdout) }
	if len(os.Args) > 1 {
		if os.Args[1] != "healthcheck" {
			fmt.Fprintf(os.Stderr, "antifraude: subcomando desconhecido %q (use healthcheck)\n", os.Args[1])
			stop()
			os.Exit(2)
		}
		// Usado pelo HEALTHCHECK da imagem final, que não tem shell nem curl.
		cmd, runFn = "antifraude healthcheck", func() error { return runHealthcheck(ctx, os.LookupEnv) }
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

// run monta e executa o serviço até ctx ser cancelado.
func run(ctx context.Context, lookup config.Lookup, stdout io.Writer) error {
	cfg, err := config.Load(lookup)
	if err != nil {
		return err
	}

	hostname, _ := os.Hostname() // em contêiner é o ID do contêiner: único por réplica
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

	// Redis com spans e latência de cada comando. Os spans não levam os argumentos dos comandos
	// (chaves com ID de conta, valores): o ID vai como atributo do span do Evaluate.
	rdb := redisvelocity.NewClient(cfg.RedisAddr, cfg.RedisTimeout)
	defer func() { _ = rdb.Close() }()
	if err := redisotel.InstrumentTracing(rdb,
		redisotel.WithTracerProvider(tel.Tracer), redisotel.WithDBStatement(false)); err != nil {
		return fmt.Errorf("instrumentando o Redis (traces): %w", err)
	}
	if err := redisotel.InstrumentMetrics(rdb, redisotel.WithMeterProvider(tel.Meter)); err != nil {
		return fmt.Errorf("instrumentando o Redis (métricas): %w", err)
	}

	inj := faults.New(cfg.Faults)
	observer, err := riskotel.New(tel.Meter, inj)
	if err != nil {
		return fmt.Errorf("métricas de negócio: %w", err)
	}
	svc := risk.NewService(redisvelocity.New(rdb), cfg.Rules, cfg.RedisTimeout, risk.WithObserver(observer))

	healthSrv := health.NewServer()
	srv := grpcserver.NewServer(grpcserver.ServerOptions{
		Logger: logger, TracerProvider: tel.Tracer, MeterProvider: tel.Meter, Propagator: tel.Propagator,
		Interceptors: []grpc.UnaryServerInterceptor{grpcapi.ErrorInterceptor(logger)},
	})
	grpcapi.Register(srv, grpcapi.NewHandler(svc, inj))
	grpcserver.RegisterPing(srv, grpcserver.NewPingHandler(serviceName))
	healthpb.RegisterHealthServer(srv, healthSrv)

	// O /readyz NÃO depende do Redis: sem ele, o antifraude continua atendendo, degradado
	// (ADR-0008, seção 3). Tirá-lo do balanceador por causa do Redis transformaria uma
	// degradação em indisponibilidade.
	adminSrv := admin.New(logger)

	logger.InfoContext(ctx, "serviço iniciando",
		"version", version, "environment", cfg.Environment,
		"grpc_addr", grpcLis.Addr().String(), "admin_addr", adminLis.Addr().String(),
		"redis_addr", cfg.RedisAddr, "redis_timeout", cfg.RedisTimeout.String(),
		"fault_latency", cfg.Faults.Latency.String(), "fault_error_rate", cfg.Faults.ErrorRate,
		"otlp_export", cfg.OTLPEndpoint != "")

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return grpcserver.Serve(gctx, srv, grpcLis, cfg.ShutdownTimeout) })
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
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := tel.Shutdown(flushCtx); err != nil {
		fmt.Fprintln(os.Stderr, "antifraude: descarregando telemetria:", err)
	}
	return runErr
}
