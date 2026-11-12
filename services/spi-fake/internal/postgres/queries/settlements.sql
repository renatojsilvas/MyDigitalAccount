-- name: InsertSettlement :execrows
-- ON CONFLICT DO NOTHING: um ID já registrado não é sobrescrito (o resultado nunca muda); zero
-- linhas afetadas = o ID já existia, e quem chama lê o registrado.
INSERT INTO settlements (settlement_id, transfer_id, payer_account_id, payee_account_id,
                         amount_cents, status, reject_reason, decided_at)
VALUES (@settlement_id, @transfer_id, @payer_account_id, @payee_account_id,
        @amount_cents, @status, sqlc.narg(reject_reason), @decided_at)
ON CONFLICT (settlement_id) DO NOTHING;

-- name: GetSettlement :one
SELECT settlement_id, transfer_id, payer_account_id, payee_account_id, amount_cents,
       status, reject_reason, decided_at
FROM settlements
WHERE settlement_id = @settlement_id;
