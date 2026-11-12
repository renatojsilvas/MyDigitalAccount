-- name: ClaimIdempotencyKey :execrows
-- Primeiro comando do aceite. Se outra transação já inseriu a mesma chave e ainda não terminou,
-- espera por ela no índice único; zero linhas = a chave já existe.
INSERT INTO transfer_idempotency_keys (account_id, key, request_hmac, transfer_id)
VALUES (@account_id, @key, @request_hmac, @transfer_id)
ON CONFLICT (account_id, key) DO NOTHING;

-- name: GetIdempotencyKey :one
SELECT request_hmac, transfer_id
FROM transfer_idempotency_keys
WHERE account_id = @account_id AND key = @key;

-- name: InsertTransfer :exec
INSERT INTO transfers (id, source_account_id, destination_key_type, destination_key, amount_cents, description,
                       state, settlement_id, limit_day, trace_carrier, next_run_at, created_at, updated_at)
VALUES (@id, @source_account_id, @destination_key_type, @destination_key, @amount_cents, @description,
        @state, @settlement_id, @limit_day, @trace_carrier, @next_run_at, @created_at, @updated_at);

-- name: InsertHistory :exec
INSERT INTO transfer_state_history (transfer_id, from_state, to_state, reason, at)
VALUES (@transfer_id, sqlc.narg(from_state), @to_state, sqlc.narg(reason), @at);

-- name: GetTransfer :one
SELECT id, source_account_id, destination_key_type, destination_key, destination_account_id, amount_cents,
       description, state, reason, settlement_id, debit_entry_id, limit_day, limit_reserved,
       settlement_maybe_sent, confirmation_overdue, trace_carrier, created_at, updated_at,
       pending_since, confirmation_attempts
FROM transfers
WHERE id = @id;

-- name: ListHistory :many
SELECT from_state, to_state, reason, at
FROM transfer_state_history
WHERE transfer_id = @transfer_id
ORDER BY id;

-- name: UsedOn :one
SELECT coalesce((SELECT used_cents FROM daily_limit_usage WHERE account_id = @account_id AND day = @day), 0)::bigint;

-- name: AcquireLease :one
-- Toma o lease de uma transferência não terminal cujo lease está livre ou expirado. O token
-- cresce a cada tomada: é o fencing token (ADR-0007, seção 4).
UPDATE transfers
SET lease_owner = @owner, lease_until = now() + @ttl::interval, lease_token = lease_token + 1
WHERE id = @id
  AND state NOT IN ('completed', 'reversed', 'rejected')
  AND (lease_until IS NULL OR lease_until < now())
RETURNING lease_token;

-- name: LockLeased :one
-- Trava a transferência só se o lease ainda for de quem pede; nenhuma linha = lease perdido.
SELECT state, source_account_id, amount_cents, limit_day, limit_reserved
FROM transfers
WHERE id = @id AND lease_token = @lease_token
FOR UPDATE;

-- name: EnsureUsageRow :exec
INSERT INTO daily_limit_usage (account_id, day, used_cents)
VALUES (@account_id, @day, 0)
ON CONFLICT (account_id, day) DO NOTHING;

-- name: ReserveUsage :execrows
-- Reserva atômica: soma só se couber no limite; zero linhas = limite estourado.
UPDATE daily_limit_usage
SET used_cents = used_cents + @amount_cents
WHERE account_id = @account_id AND day = @day AND used_cents + @amount_cents <= @daily_limit_cents;

-- name: ReleaseUsage :execrows
-- Devolve ao dia da reserva (limit_day da transferência), nunca ao dia corrente.
UPDATE daily_limit_usage
SET used_cents = used_cents - @amount_cents
WHERE account_id = @account_id AND day = @day;

-- name: MarkLimitReserved :execrows
UPDATE transfers
SET limit_reserved = @limit_reserved, updated_at = @updated_at
WHERE id = @id AND lease_token = @lease_token;

-- name: TransitionTransfer :execrows
-- Compare-and-set com fencing: só transita se o estado for o esperado e o lease ainda for de
-- quem pede (ADR-0007, seção 2). Zero linhas = transição perdida.
UPDATE transfers
SET state = @to_state,
    reason = sqlc.narg(reason),
    destination_account_id = coalesce(sqlc.narg(destination_account_id), destination_account_id),
    debit_entry_id = coalesce(sqlc.narg(debit_entry_id), debit_entry_id),
    pending_since = coalesce(sqlc.narg(pending_since), pending_since),
    next_run_at = @next_run_at,
    updated_at = @updated_at
WHERE id = @id AND state = @from_state AND lease_token = @lease_token;

-- name: ClaimDue :many
-- Fila sobre o Postgres (ADR-0007, seção 4): pega até max_jobs transferências vencidas nos
-- estados pedidos, cujo lease está livre ou expirado. SKIP LOCKED deixa várias instâncias
-- pegarem trabalho ao mesmo tempo sem disputar a mesma linha; o token cresce a cada tomada.
WITH due AS (
    SELECT id FROM transfers
    WHERE state = ANY(@states::text[])
      AND next_run_at <= now()
      AND (lease_until IS NULL OR lease_until < now())
    ORDER BY next_run_at
    LIMIT @max_jobs
    FOR UPDATE SKIP LOCKED
)
UPDATE transfers t
SET lease_owner = @owner, lease_until = now() + @ttl::interval, lease_token = t.lease_token + 1
FROM due
WHERE t.id = due.id
RETURNING t.id, t.source_account_id, t.destination_key_type, t.destination_key, t.destination_account_id,
          t.amount_cents, t.description, t.state, t.reason, t.settlement_id, t.debit_entry_id, t.limit_day,
          t.limit_reserved, t.settlement_maybe_sent, t.confirmation_overdue, t.trace_carrier, t.created_at,
          t.updated_at, t.pending_since, t.confirmation_attempts, t.lease_token;

-- name: RenewLease :execrows
UPDATE transfers SET lease_until = now() + @ttl::interval
WHERE id = @id AND lease_token = @lease_token;

-- name: ReleaseLease :execrows
-- Solta o lease (o token fica: só cresce) e agenda a próxima rodada.
UPDATE transfers SET lease_owner = NULL, lease_until = NULL, next_run_at = @next_run_at
WHERE id = @id AND lease_token = @lease_token;

-- name: MarkMaybeSent :execrows
UPDATE transfers SET settlement_maybe_sent = true, updated_at = @updated_at
WHERE id = @id AND lease_token = @lease_token;

-- name: RecordConfirmationPoll :execrows
-- Consulta de status sem resposta definitiva: conta a tentativa, marca o prazo estourado (só vai
-- de falso para verdadeiro), agenda a próxima e solta o lease.
UPDATE transfers
SET confirmation_attempts = confirmation_attempts + 1,
    confirmation_overdue = confirmation_overdue OR @overdue,
    next_run_at = @next_run_at, lease_owner = NULL, lease_until = NULL, updated_at = @updated_at
WHERE id = @id AND lease_token = @lease_token AND state = 'pending_confirmation';

-- name: PendingStats :one
SELECT count(*)::bigint AS pending,
       count(*) FILTER (WHERE confirmation_overdue)::bigint AS overdue,
       coalesce(extract(epoch FROM now() - min(pending_since)), 0)::float8 AS oldest_age_seconds
FROM transfers
WHERE state = 'pending_confirmation';
