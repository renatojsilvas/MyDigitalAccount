// Package account é o domínio do ledger: contas, dinheiro e lançamentos em partida dobrada
// (ADR-0004). Não depende de banco, gRPC nem framework; tempo e identificadores chegam
// como parâmetros, para que as regras sejam determinísticas e testáveis.
package account

import (
	"errors"
	"fmt"
)

// Erros de domínio. A borda gRPC os traduz para status e ErrorInfo (ADR-0004, seção 5).
var (
	// ErrAccountNotFound indica que a conta informada não existe.
	ErrAccountNotFound = errors.New("account: not found")
	// ErrAccountAlreadyExists indica que o documento (CPF) já tem conta.
	ErrAccountAlreadyExists = errors.New("account: document already has an account")
	// ErrInsufficientFunds indica que o saldo não cobre o débito.
	ErrInsufficientFunds = errors.New("account: insufficient funds")
)

// Violações de invariante: indicam bug ou limite físico, nunca um pedido comum do cliente.
var (
	// ErrMoneyOverflow indica que uma soma ou subtração sairia da faixa de int64.
	ErrMoneyOverflow = errors.New("account: money overflow")
	// ErrInvalidEntry indica um lançamento que não respeita a partida dobrada.
	ErrInvalidEntry = errors.New("account: invalid entry")
)

// ValidationError descreve um campo de pedido fora do formato aceito.
type ValidationError struct {
	Field   string
	Problem string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("account: invalid %s: %s", e.Field, e.Problem)
}

func invalid(field, problem string) error {
	return &ValidationError{Field: field, Problem: problem}
}
