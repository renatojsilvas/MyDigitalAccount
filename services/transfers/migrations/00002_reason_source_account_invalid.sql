-- Motivo novo de recusa: a conta de origem não existe no ledger (o débito volta
-- ACCOUNT_NOT_FOUND). Só para frente: amplia o CHECK da 00001, já aplicada.

-- +goose Up
ALTER TABLE transfers DROP CONSTRAINT transfers_reason_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_reason_check CHECK (reason IN (
    'antifraud_denied', 'limit_exceeded', 'destination_invalid', 'same_account', 'insufficient_funds',
    'dependency_unavailable', 'settlement_rejected', 'settlement_not_sent', 'source_account_invalid'));
