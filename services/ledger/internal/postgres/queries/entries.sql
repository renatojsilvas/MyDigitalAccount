-- name: InsertJournalEntry :exec
INSERT INTO journal_entries (id, description, posted_at)
VALUES (@id, @description, @posted_at);

-- name: InsertPosting :exec
INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents)
VALUES (@entry_id, @account_id, @direction, @amount_cents, sqlc.narg(balance_after_cents));

-- name: ListStatement :many
-- Pernas da conta da mais recente para a mais antiga, a partir do cursor (exclusivo).
-- Usa o índice (account_id, id DESC): cada página é uma leitura de faixa, sem OFFSET.
SELECT p.id, p.entry_id, p.direction, p.amount_cents, p.balance_after_cents,
       e.description, e.posted_at
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
