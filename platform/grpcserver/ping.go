package grpcserver

import (
	"context"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pingv1 "conta.local/gen/go/conta/ping/v1"
)

const maxPingMessageLen = 256

// PingHandler implementa conta.ping.v1.PingService. Existe só para validar a fundação.
type PingHandler struct {
	pingv1.UnimplementedPingServiceServer
	service string
}

// NewPingHandler cria o handler; service é o nome devolvido na resposta.
func NewPingHandler(service string) *PingHandler { return &PingHandler{service: service} }

// RegisterPing aceita a interface gerada, não o tipo concreto, para os testes poderem
// registrar dublês.
func RegisterPing(srv grpc.ServiceRegistrar, h pingv1.PingServiceServer) {
	pingv1.RegisterPingServiceServer(srv, h)
}

// Ping devolve o eco da mensagem e o nome do serviço.
func (h *PingHandler) Ping(_ context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	if utf8.RuneCountInString(req.GetMessage()) > maxPingMessageLen {
		return nil, status.Errorf(codes.InvalidArgument, "message excede %d caracteres", maxPingMessageLen)
	}
	return &pingv1.PingResponse{Message: req.GetMessage(), Service: h.service}, nil
}
