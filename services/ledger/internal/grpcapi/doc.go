// Package grpcapi é a borda gRPC do ledger: traduz erros de domínio para status gRPC e
// implementa os handlers. É a única camada do ledger que conhece códigos gRPC; o servidor em
// si (interceptors comuns, desligamento gracioso) vem de conta.local/platform/grpcserver.
package grpcapi
