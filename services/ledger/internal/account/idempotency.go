package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const maxIdempotencyKeyLen = 128

// Escopos de idempotência: a mesma chave em operações diferentes são chaves diferentes.
const (
	scopeOpenAccount = "open_account"
	scopeCredit      = "credit"
	scopeDebit       = "debit"
)

// IdempotencyKey é a chave vinda do cliente: de 1 a 128 caracteres ASCII visíveis.
type IdempotencyKey struct {
	value string
}

// NewIdempotencyKey valida a chave. Restrita a ASCII visível para caber em log e em header
// sem escape e para não haver duas grafias Unicode da "mesma" chave.
func NewIdempotencyKey(s string) (IdempotencyKey, error) {
	if s == "" {
		return IdempotencyKey{}, invalid("idempotency_key", "is required")
	}
	if len(s) > maxIdempotencyKeyLen {
		return IdempotencyKey{}, invalid("idempotency_key", "is too long")
	}
	for i := range len(s) {
		if s[i] < '!' || s[i] > '~' {
			return IdempotencyKey{}, invalid("idempotency_key", "must have only visible ASCII characters")
		}
	}
	return IdempotencyKey{value: s}, nil
}

func (k IdempotencyKey) String() string {
	return k.value
}

// requestHash resume os campos do pedido em SHA-256. Cada campo entra com o tamanho na
// frente, para que ("ab", "c") e ("a", "bc") não deem o mesmo hash. O hash é feito sobre os
// valores do domínio, e não sobre os bytes do Protobuf, que não têm serialização canônica.
func requestHash(fields ...string) []byte {
	h := sha256.New()
	var size [4]byte
	for _, f := range fields {
		binary.BigEndian.PutUint32(size[:], uint32(len(f))) //nolint:gosec // campos curtos, validados antes
		h.Write(size[:])
		h.Write([]byte(f))
	}
	return h.Sum(nil)
}

// encoder e decoder são as duas metades do ResultCodec para o tipo de resultado T.
type (
	encoder[T any] func(T) ([]byte, error)
	decoder[T any] func([]byte) (T, error)
)

// idempotent executa op uma única vez por (scope, key), numa transação (ADR-0004, seção 3):
//
//  1. reserva a chave antes de qualquer outro trabalho;
//  2. se ela já existe: com o mesmo hash, devolve o resultado gravado; com outro, recusa
//     com ErrIdempotencyKeyReused;
//  3. se é nova: executa op e grava o resultado, inclusive uma recusa de negócio, com commit.
//
// Um erro que não é recusa de negócio desfaz tudo, inclusive a reserva da chave. O Outcome
// diz o que aconteceu (vazio quando houve rollback), para a observabilidade.
func idempotent[T any](
	ctx context.Context, s *Service, scope string, key IdempotencyKey, hash []byte,
	enc encoder[T], dec decoder[T], op func(Tx) (T, error),
) (T, Outcome, error) {
	var (
		result    T
		rejection error
		outcome   Outcome
	)
	err := s.store.WithTx(ctx, func(tx Tx) error {
		claimed, err := tx.ClaimKey(ctx, scope, key.String(), hash)
		if err != nil {
			return err
		}
		if !claimed {
			result, rejection, err = replay(ctx, tx, s.codec, scope, key, hash, dec)
			outcome = OutcomeReplayed
			if errors.Is(rejection, ErrIdempotencyKeyReused) {
				outcome = OutcomeKeyConflict
			}
			return err
		}

		out, opErr := op(tx)
		if opErr != nil {
			if !isBusinessRejection(opErr) {
				return opErr
			}
			code, body, err := s.codec.EncodeRejection(opErr)
			if err != nil {
				return fmt.Errorf("account: codificando recusa: %w", err)
			}
			rejection, outcome = opErr, OutcomeRejected
			return tx.CompleteKey(ctx, scope, key.String(), code, body)
		}
		body, err := enc(out)
		if err != nil {
			return fmt.Errorf("account: codificando resultado: %w", err)
		}
		result, outcome = out, OutcomeExecuted
		return tx.CompleteKey(ctx, scope, key.String(), 0, body)
	})
	var zero T
	if err != nil {
		// Rollback: nada foi gravado, não há desfecho de negócio.
		return zero, "", err
	}
	if rejection != nil {
		return zero, outcome, rejection
	}
	return result, outcome, nil
}

// replay devolve o resultado gravado de uma chave já usada.
func replay[T any](
	ctx context.Context, tx Tx, codec ResultCodec, scope string, key IdempotencyKey, hash []byte, dec decoder[T],
) (T, error, error) {
	var zero T
	rec, err := tx.KeyRecord(ctx, scope, key.String())
	if err != nil {
		return zero, nil, err
	}
	if !bytes.Equal(rec.RequestHash, hash) {
		return zero, ErrIdempotencyKeyReused, nil
	}
	if !rec.Completed {
		// Uma reserva de outra transação só fica visível depois do commit, já completa.
		return zero, nil, errors.New("account: chave de idempotência pendente fora da própria transação")
	}
	if rec.StatusCode != 0 {
		return zero, codec.DecodeRejection(rec.StatusCode, rec.Body), nil
	}
	out, err := dec(rec.Body)
	if err != nil {
		return zero, nil, fmt.Errorf("account: decodificando resultado gravado: %w", err)
	}
	return out, nil, nil
}
