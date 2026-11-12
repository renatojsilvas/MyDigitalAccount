-- Motivo, referência externa e estorno nos lançamentos (ADR-0009).
--
-- Roda com o papel ledger_owner. É a única migration que altera linhas de journal_entries:
-- preenche o motivo dos lançamentos já gravados antes de torná-lo obrigatório. O papel do
-- serviço (ledger_app) continua sem UPDATE na tabela.

-- +goose Up

ALTER TABLE journal_entries
    ADD COLUMN reason             text,
    -- Formato igual ao do domínio (account.ExternalReference): 1 a 64 caracteres ASCII visíveis.
    ADD COLUMN external_reference text CHECK (external_reference ~ '^[!-~]{1,64}$'),
    -- UNIQUE é a última barreira contra o estorno duplo: o domínio confere antes, sob o lock
    -- da conta, mas se errar, o segundo estorno falha aqui e a transação é desfeita.
    ADD COLUMN reverses_entry_id  uuid UNIQUE REFERENCES journal_entries (id);

-- O motivo dos lançamentos antigos sai da contrapartida, que é uma por motivo (ADR-0004).
UPDATE journal_entries e
   SET reason = CASE
           WHEN EXISTS (SELECT 1 FROM postings p
                         WHERE p.entry_id = e.id
                           AND p.account_id = '00000000-0000-0000-0000-000000000001') THEN 'opening'
           ELSE 'external'
       END;

ALTER TABLE journal_entries
    ALTER COLUMN reason SET NOT NULL,
    ADD CONSTRAINT journal_entries_reason CHECK (reason IN ('opening', 'external', 'reversal')),
    ADD CONSTRAINT journal_entries_reversal_target CHECK ((reason = 'reversal') = (reverses_entry_id IS NOT NULL));
