-- Worker de confirmação (ADR-0007, seção 5): quando o Pix entrou em PENDING_CONFIRMATION (o
-- prazo máximo conta daí) e quantas consultas de status já foram feitas (o intervalo cresce).

-- +goose Up
ALTER TABLE transfers
    ADD COLUMN pending_since         timestamptz,
    ADD COLUMN confirmation_attempts integer NOT NULL DEFAULT 0 CHECK (confirmation_attempts >= 0);

-- Métricas de pendentes (quantos, idade do mais antigo) sem varrer a tabela inteira.
CREATE INDEX transfers_pending ON transfers (pending_since) WHERE state = 'pending_confirmation';

GRANT UPDATE (pending_since, confirmation_attempts) ON transfers TO transfers_app;
