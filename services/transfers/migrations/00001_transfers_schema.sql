-- Schema do transfers (ADR-0007). Migrations só para frente, como no ledger.
--
-- Depende do papel transfers_app, criado fora daqui (job postgres-bootstrap no ambiente,
-- testkit nos testes): papéis são do cluster, não do banco.

-- +goose Up

CREATE TABLE transfers (
    id                     uuid        PRIMARY KEY,
    source_account_id      uuid        NOT NULL,
    -- A chave de destino é dado de negócio (como num comprovante); logs, traces e métricas só
    -- a veem mascarada. A tabela de idempotência não a guarda (ADR-0007, seção 4).
    destination_key_type   text        NOT NULL CHECK (destination_key_type IN ('cpf')),
    destination_key        text        NOT NULL CHECK (destination_key ~ '^[0-9]{11}$'),
    destination_account_id uuid,
    amount_cents           bigint      NOT NULL CHECK (amount_cents > 0),
    description            text        NOT NULL,
    state                  text        NOT NULL CHECK (state IN ('accepted', 'validated', 'debited', 'settling',
                                           'pending_confirmation', 'settled', 'completed', 'reversing', 'reversed',
                                           'rejected')),
    reason                 text        CHECK (reason IN ('antifraud_denied', 'limit_exceeded', 'destination_invalid',
                                           'same_account', 'insufficient_funds', 'dependency_unavailable',
                                           'settlement_rejected', 'settlement_not_sent')),
    -- Gerado no aceite e persistido antes de qualquer chamada ao SPI (seção 4.5 do plano).
    settlement_id          uuid        NOT NULL UNIQUE,
    debit_entry_id         uuid,
    -- Dia (America/Sao_Paulo) da reserva do limite; a devolução vai sempre para ele.
    limit_day              date        NOT NULL,
    limit_reserved         boolean     NOT NULL DEFAULT false,
    -- Alguma tentativa de liquidação pode ter saído; nunca volta a falso (trigger abaixo).
    settlement_maybe_sent  boolean     NOT NULL DEFAULT false,
    confirmation_overdue   boolean     NOT NULL DEFAULT false,
    -- Contexto de trace do aceite (carrier W3C), para a saga continuar o mesmo trace.
    trace_carrier          jsonb       NOT NULL DEFAULT '{}',
    -- Fila sobre o Postgres e lease do runner (ADR-0007, seção 4). lease_token é o fencing
    -- token: cresce a cada tomada, e toda escrita do runner confere o seu.
    next_run_at            timestamptz NOT NULL,
    lease_owner            text,
    lease_until            timestamptz,
    lease_token            bigint      NOT NULL DEFAULT 0,
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,

    -- Só recusas e estornos têm motivo, e todos têm.
    CONSTRAINT transfers_reason_by_state CHECK (
        (state IN ('rejected', 'reversing', 'reversed')) = (reason IS NOT NULL)),
    CONSTRAINT transfers_debit_after_debited CHECK (
        state IN ('accepted', 'validated', 'rejected') OR debit_entry_id IS NOT NULL)
);

-- Fila do runner: só as não terminais, por vencimento.
CREATE INDEX transfers_runnable ON transfers (next_run_at)
    WHERE state NOT IN ('completed', 'reversed', 'rejected');
-- Consulta da conta (GetPix confere o dono).
CREATE INDEX transfers_by_source ON transfers (source_account_id, created_at DESC);

-- settlement_maybe_sent é irreversível (ADR-0007, seção 5): se o domínio errar, o banco recusa.
-- +goose StatementBegin
CREATE FUNCTION transfers_maybe_sent_is_sticky() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.settlement_maybe_sent AND NOT NEW.settlement_maybe_sent THEN
        RAISE EXCEPTION 'transferência %: settlement_maybe_sent não pode voltar a falso', OLD.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'transfers_maybe_sent_sticky';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER transfers_maybe_sent_sticky
    BEFORE UPDATE OF settlement_maybe_sent ON transfers
    FOR EACH ROW EXECUTE FUNCTION transfers_maybe_sent_is_sticky();

-- Histórico de estados (GetPix). Append-only.
CREATE TABLE transfer_state_history (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transfer_id uuid        NOT NULL REFERENCES transfers (id),
    from_state  text,
    to_state    text        NOT NULL,
    reason      text,
    at          timestamptz NOT NULL
);
CREATE INDEX transfer_state_history_by_transfer ON transfer_state_history (transfer_id, id);

-- Idempotência do aceite, com escopo pela conta de origem. Sem dado pessoal: só o HMAC do
-- pedido e o ID da transferência (ADR-0007, seção 4).
CREATE TABLE transfer_idempotency_keys (
    account_id   uuid        NOT NULL,
    key          text        NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
    request_hmac bytea       NOT NULL CHECK (length(request_hmac) = 32),
    -- Adiada: o aceite reserva a chave antes de gravar a transferência (um pedido concorrente
    -- com a mesma chave espera no índice único), e a FK é conferida no COMMIT.
    transfer_id  uuid        NOT NULL REFERENCES transfers (id) DEFERRABLE INITIALLY DEFERRED,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, key)
);

-- Limite diário reservado por conta e dia (ADR-0007, seção 6). O CHECK impede devolver mais
-- do que foi reservado.
CREATE TABLE daily_limit_usage (
    account_id uuid   NOT NULL,
    day        date   NOT NULL,
    used_cents bigint NOT NULL CHECK (used_cents >= 0),
    PRIMARY KEY (account_id, day)
);

-- Privilégios do serviço, tabela a tabela. Na transferência, só as colunas da saga mudam:
-- origem, destino informado, valor, settlement_id e o dia do limite são imutáveis.
GRANT SELECT, INSERT ON transfers TO transfers_app;
GRANT UPDATE (destination_account_id, state, reason, debit_entry_id, limit_reserved, settlement_maybe_sent,
              confirmation_overdue, next_run_at, lease_owner, lease_until, lease_token, updated_at)
    ON transfers TO transfers_app;
GRANT SELECT, INSERT ON transfer_state_history TO transfers_app;
GRANT SELECT, INSERT ON transfer_idempotency_keys TO transfers_app;
GRANT SELECT, INSERT ON daily_limit_usage TO transfers_app;
GRANT UPDATE (used_cents) ON daily_limit_usage TO transfers_app;
