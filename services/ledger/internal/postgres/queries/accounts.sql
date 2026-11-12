-- name: InsertCustomerAccount :one
-- ON CONFLICT DO NOTHING em vez de deixar o UNIQUE estourar: a violação abortaria a
-- transação e a recusa não poderia ser gravada na tabela de idempotência (ADR-0004, seção 4).
-- Nenhuma linha devolvida = CPF já cadastrado.
INSERT INTO accounts (id, kind, holder_name, holder_document, status, balance_cents, created_at)
VALUES (@id, 'customer', @holder_name, @holder_document, @status, 0, @created_at)
ON CONFLICT (holder_document) DO NOTHING
RETURNING id;

-- name: GetCustomerAccount :one
SELECT id, holder_name, holder_document, status, balance_cents, created_at
FROM accounts
WHERE id = @id AND kind = 'customer';

-- name: GetCustomerAccountByDocument :one
-- O UNIQUE de holder_document já é o índice desta busca.
SELECT id, holder_name, holder_document, status, balance_cents, created_at
FROM accounts
WHERE holder_document = @holder_document::text AND kind = 'customer';

-- name: LockCustomerAccount :one
-- Lock pessimista (ADR-0004, seção 2): o segundo débito na mesma conta espera aqui até o
-- primeiro terminar, e então lê o saldo já atualizado.
SELECT id, holder_name, holder_document, status, balance_cents, created_at
FROM accounts
WHERE id = @id AND kind = 'customer'
FOR UPDATE;

-- name: UpdateCustomerBalance :execrows
UPDATE accounts
SET balance_cents = @balance_cents
WHERE id = @id AND kind = 'customer';
