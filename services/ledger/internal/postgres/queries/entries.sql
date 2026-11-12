-- name: InsertJournalEntry :exec
INSERT INTO journal_entries (id, description, posted_at, reason, external_reference, reverses_entry_id)
VALUES (@id, @description, @posted_at, @reason, sqlc.narg(external_reference), sqlc.narg(reverses_entry_id));

-- name: InsertPosting :exec
INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents)
VALUES (@entry_id, @account_id, @direction, @amount_cents, sqlc.narg(balance_after_cents));

-- name: ListStatement :many
-- Pernas da conta da mais recente para a mais antiga, a partir do cursor (exclusivo).
-- Usa o índice (account_id, id DESC): cada página é uma leitura de faixa, sem OFFSET.
SELECT p.id, p.entry_id, p.direction, p.amount_cents, p.balance_after_cents,
       e.description, e.posted_at, e.reason, e.external_reference, e.reverses_entry_id
FROM postings p
JOIN journal_entries e ON e.id = p.entry_id
WHERE p.account_id = @account_id AND p.id < @before_id
ORDER BY p.id DESC
LIMIT @page_size;

-- name: NetPostings :one
-- Créditos menos débitos de uma conta, recalculados das pernas. Para contas de cliente deve
-- ser igual ao saldo gravado (invariante conferida nos testes e, na Fase 5, pela conciliação).
SELECT coalesce(sum(CASE direction WHEN 'credit' THEN amount_cents ELSE -amount_cents END), 0)::bigint AS net_cents
FROM postings
WHERE account_id = @account_id;

-- name: GetEntryLegs :many
-- Lançamento e as suas pernas, para o estorno. customer diz qual perna é de conta de cliente.
SELECT e.id AS entry_id, e.reason, p.account_id, p.direction, p.amount_cents,
       (a.kind = 'customer')::boolean AS customer
FROM journal_entries e
JOIN postings p ON p.entry_id = e.id
JOIN accounts a ON a.id = p.account_id
WHERE e.id = @id
ORDER BY p.id;

-- name: EntryIsReversed :one
-- Usa o índice do UNIQUE de reverses_entry_id.
SELECT EXISTS (SELECT 1 FROM journal_entries WHERE reverses_entry_id = @id::uuid)::boolean AS reversed;
