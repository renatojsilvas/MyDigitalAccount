// Package postgres é o adaptador de persistência do ledger: traduz entre os tipos do domínio
// (pacote account) e as tabelas, usando as queries geradas pelo sqlc, e traduz os erros do
// Postgres para erros de domínio ou para erros que se pode repetir.
package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// Códigos SQLSTATE de erros transitórios: repetir a operação inteira pode dar certo.
const (
	codeLockNotAvailable     = "55P03" // lock_timeout estourou
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
)

// errCorruptRow indica uma linha que não passa nas validações do domínio: dado gravado fora
// das regras, nunca culpa de quem chamou.
var errCorruptRow = errors.New("postgres: linha inválida no banco")

// RetryableError marca uma falha transitória (lock timeout, deadlock, falha de
// serialização). Quem trata erros pergunta pelo comportamento, Retryable(), sem importar
// este pacote; a borda gRPC o traduz para UNAVAILABLE.
type RetryableError struct {
	err error
}

func (e *RetryableError) Error() string { return "postgres: falha transitória: " + e.err.Error() }

// Unwrap expõe o erro original do Postgres.
func (e *RetryableError) Unwrap() error { return e.err }

// Retryable informa que a operação pode ser repetida (com a mesma chave de idempotência).
func (e *RetryableError) Retryable() bool { return true }

// classify embrulha os erros transitórios do Postgres em RetryableError.
func classify(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeLockNotAvailable, codeSerializationFailure, codeDeadlockDetected:
			return &RetryableError{err: err}
		}
	}
	return err
}
