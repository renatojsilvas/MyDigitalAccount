package clients_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	spiv1 "conta.local/gen/go/conta/spi/v1"
	"conta.local/testkit/toxi"
	"conta.local/transfers/internal/clients"
	"conta.local/transfers/internal/resilience"
	"conta.local/transfers/internal/saga"
)

// recordingSPI registra cada liquidação que chega (liquida tudo) e responde a consulta.
type recordingSPI struct {
	spiv1.UnimplementedSpiServiceServer
	mu      sync.Mutex
	settled map[string]bool
}

func (s *recordingSPI) Settle(_ context.Context, req *spiv1.SettleRequest) (*spiv1.SettleResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled[req.GetSettlementId()] = true
	return &spiv1.SettleResponse{Settlement: &spiv1.Settlement{Status: spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED}}, nil
}

func (s *recordingSPI) GetSettlement(_ context.Context, req *spiv1.GetSettlementRequest) (*spiv1.GetSettlementResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.settled[req.GetSettlementId()] {
		return nil, withReason(codes.NotFound, "ERROR_REASON_SETTLEMENT_NOT_FOUND")
	}
	return &spiv1.GetSettlementResponse{Settlement: &spiv1.Settlement{Status: spiv1.SettlementStatus_SETTLEMENT_STATUS_SETTLED}}, nil
}

// ownIP devolve o IP deste contêiner na rede do compose, que o Toxiproxy alcança.
func ownIP(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	t.Skip("sem IP de rede para o Toxiproxy alcançar este processo")
	return ""
}

// Falhas de rede reais entre transfers e SPI (ADR-0003: Toxiproxy), pelo adaptador de produção.
func TestSPIThroughNetworkFailures(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(t.Context(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &recordingSPI{settled: map[string]bool{}}
	srv := grpc.NewServer()
	spiv1.RegisterSpiServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	proxy := toxi.New(t, net.JoinHostPort(ownIP(t), port))

	conn, err := grpc.NewClient(proxy.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	o := newObs()
	spi := clients.NewSPI(spiv1.NewSpiServiceClient(conn), clients.NewDependency("spi", resilience.BreakerConfig{
		ConsecutiveFailures: 3, FailureRatio: 1, MinRequests: 100, Window: time.Minute, OpenFor: time.Minute,
	}, resilience.Policy{Attempts: 1}, o))
	settle := func(t *testing.T, req saga.SettleRequest, marked *bool) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		_, err := spi.Settle(ctx, req, func() error { *marked = true; return nil })
		return err
	}

	t.Run("resposta depois do prazo: liquidou, e só a consulta mostra", func(t *testing.T) {
		// A conexão precisa estar aberta: numa conexão nova, o cliente gRPC espera o preface do
		// servidor antes de mandar o pedido, e a latência faria o prazo estourar antes de o pedido
		// sair (o SPI nunca o veria). Do lado do cliente os dois casos são indistinguíveis, por
		// isso ambos são ambíguos; aqui o teste quer o caso "chegou, mas a resposta não voltou".
		if _, err := spi.Status(t.Context(), uuid.New()); !errors.Is(err, saga.ErrSettlementNotFound) {
			t.Fatalf("aquecimento: %v", err)
		}
		req := settleRequest()
		proxy.Latency(t, time.Second)
		var marked bool
		if err := settle(t, req, &marked); status.Code(err) != codes.DeadlineExceeded || !marked {
			t.Fatalf("err = %v, marcado=%v; want timeout (ambíguo) com o marcador gravado", err, marked)
		}
		proxy.RemoveToxics(t)
		// O pedido chegou ao SPI apesar do timeout: é o caso que a seção 4.5 do plano descreve.
		deadline := time.Now().Add(3 * time.Second)
		for {
			st, err := spi.Status(t.Context(), req.SettlementID)
			if err == nil && st == saga.Settled {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("consulta = %v, %v; want liquidado", st, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	t.Run("conexão recusada: ambíguo, nunca prova de não enviado", func(t *testing.T) {
		proxy.Disable(t)
		defer proxy.Enable(t)
		var marked bool
		err := settle(t, settleRequest(), &marked)
		if err == nil || errors.Is(err, resilience.ErrNotSent) || !marked {
			t.Fatalf("err = %v, marcado=%v; want erro ambíguo com o marcador gravado", err, marked)
		}
		// Mais falhas abrem o circuito; aí sim, nada sai e não há marcador.
		for range 3 {
			_ = settle(t, settleRequest(), new(bool))
		}
		marked = false
		if err := settle(t, settleRequest(), &marked); !errors.Is(err, resilience.ErrNotSent) || marked {
			t.Fatalf("circuito aberto: err = %v, marcado=%v; want ErrNotSent sem marcador", err, marked)
		}
	})

	if _, err := spi.Status(t.Context(), uuid.New()); err == nil {
		t.Fatal("consulta com o circuito aberto deveria falhar")
	}
}
