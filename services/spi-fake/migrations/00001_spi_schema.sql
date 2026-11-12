-- Schema do spi-fake (simulador do SPI). Migrations só para frente, como no ledger.
--
-- Depende do papel spi_app, criado fora daqui (job postgres-bootstrap no ambiente, testkit
-- nos testes): papéis são do cluster, não do banco.

-- +goose Up

-- Uma linha por liquidação decidida. O settlement_id é gerado por quem pede (o transfers) e é a
-- chave de idempotência: a PK garante que um ID tem um único resultado, que nunca muda.
CREATE TABLE settlements (
    settlement_id    uuid        PRIMARY KEY,
    transfer_id      uuid        NOT NULL,
    payer_account_id uuid        NOT NULL,
    payee_account_id uuid        NOT NULL,
    amount_cents     bigint      NOT NULL CHECK (amount_cents > 0),
    status           text        NOT NULL CHECK (status IN ('settled', 'rejected')),
    reject_reason    text        CHECK (reject_reason IN ('simulated')),
    decided_at       timestamptz NOT NULL,

    CONSTRAINT settlements_reject_reason CHECK ((status = 'rejected') = (reject_reason IS NOT NULL))
);

-- O resultado é imutável também por privilégio: o serviço só insere e lê.
GRANT SELECT, INSERT ON settlements TO spi_app;
