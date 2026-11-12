-- name: ClaimIdempotencyKey :one
-- Primeiro comando da transação de escrita (ADR-0004, seção 3). Se outra transação já inseriu
-- a mesma chave e ainda não terminou, este INSERT espera por ela no índice único; quando ela
-- faz commit, o ON CONFLICT entra em ação e nenhuma linha volta (a chave já existe).
INSERT INTO idempotency_keys (scope, key, request_hash)
VALUES (@scope, @key, @request_hash)
ON CONFLICT (scope, key) DO NOTHING
RETURNING scope;

-- name: GetIdempotencyKey :one
SELECT request_hash, status_code, body
FROM idempotency_keys
WHERE scope = @scope AND key = @key;

-- name: CompleteIdempotencyKey :execrows
UPDATE idempotency_keys
SET status_code = @status_code, body = @body
WHERE scope = @scope AND key = @key AND status_code IS NULL;
