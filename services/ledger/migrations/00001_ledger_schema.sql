-- Schema inicial do ledger (ADR-0004). Migrations são só para frente: não há Down, porque
-- desfazer um schema com dinheiro registrado nunca é a correção certa; corrige-se com uma
-- nova migration.
--
-- Depende do papel ledger_app, criado fora daqui (deploy/postgres/init no ambiente, testkit
-- nos testes): papéis são do cluster, não do banco.

-- +goose Up

-- Contas de cliente e internas na mesma tabela, para que toda perna tenha FK para uma conta.
-- As colunas de cliente e de conta interna são exclusivas entre si (CHECK por tipo).
CREATE TABLE accounts (
    id              uuid        PRIMARY KEY,
    kind            text        NOT NULL CHECK (kind IN ('customer', 'internal')),
    -- Nome estável das contas internas (opening_balance, settlement).
    code            text        UNIQUE,
    holder_name     text,
    -- Um CPF, uma conta. NULL (contas internas) não conflita com NULL.
    holder_document text        UNIQUE CHECK (holder_document ~ '^[0-9]{11}$'),
    status          text        NOT NULL CHECK (status IN ('active')),
    -- Saldo materializado só nas contas de cliente; o das internas é derivado das pernas.
    balance_cents   bigint,
    created_at      timestamptz NOT NULL,

    -- Rede de segurança da regra de saldo: se o domínio errar, a transação falha.
    CONSTRAINT accounts_balance_non_negative CHECK (balance_cents >= 0),
    CONSTRAINT accounts_kind_fields CHECK (
        (kind = 'customer' AND code IS NULL
            AND holder_name IS NOT NULL AND holder_document IS NOT NULL AND balance_cents IS NOT NULL)
        OR
        (kind = 'internal' AND code IS NOT NULL
            AND holder_name IS NULL AND holder_document IS NULL AND balance_cents IS NULL)
    )
);

-- Identificadores iguais aos de account.OpeningBalanceAccountID e account.SettlementAccountID.
INSERT INTO accounts (id, kind, code, status, created_at) VALUES
    ('00000000-0000-0000-0000-000000000001', 'internal', 'opening_balance', 'active', now()),
    ('00000000-0000-0000-0000-000000000002', 'internal', 'settlement',      'active', now());

CREATE TABLE journal_entries (
    id          uuid        PRIMARY KEY,
    description text        NOT NULL,
    posted_at   timestamptz NOT NULL
);

CREATE TABLE postings (
    -- Gerado pelo banco. Como a conta de cliente fica travada durante o lançamento, as pernas
    -- de uma conta são inseridas em fila, e este id segue a ordem em que o saldo mudou: é o
    -- cursor do extrato.
    id                  bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    entry_id            uuid   NOT NULL REFERENCES journal_entries (id),
    account_id          uuid   NOT NULL REFERENCES accounts (id),
    direction           text   NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_cents        bigint NOT NULL CHECK (amount_cents > 0),
    -- Saldo da conta logo depois desta perna; só nas pernas de conta de cliente.
    balance_after_cents bigint CHECK (balance_after_cents >= 0),

    UNIQUE (entry_id, account_id)
);

-- Extrato: pernas de uma conta, da mais recente para a mais antiga, paginadas por id.
CREATE INDEX postings_account_statement ON postings (account_id, id DESC);

-- Partida dobrada conferida pelo banco no COMMIT (constraint trigger adiada): durante a
-- transação o lançamento fica temporariamente incompleto, e só o estado final importa.
-- Dispara por lançamento e por perna; a perna cobre o caso de alguém acrescentar pernas a
-- um lançamento já gravado, e o lançamento cobre o caso de nenhuma perna.
-- +goose StatementBegin
CREATE FUNCTION check_entry_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_entry   uuid;
    v_debits  numeric;
    v_credits numeric;
    v_legs    integer;
BEGIN
    -- IF em vez de CASE: o PL/pgSQL só resolve NEW.entry_id no ramo que executa.
    IF TG_TABLE_NAME = 'postings' THEN
        v_entry := NEW.entry_id;
    ELSE
        v_entry := NEW.id;
    END IF;

    SELECT coalesce(sum(amount_cents) FILTER (WHERE direction = 'debit'), 0),
           coalesce(sum(amount_cents) FILTER (WHERE direction = 'credit'), 0),
           count(*)
      INTO v_debits, v_credits, v_legs
      FROM postings
     WHERE entry_id = v_entry;

    IF v_legs < 2 OR v_debits <> v_credits THEN
        RAISE EXCEPTION 'lançamento % fora da partida dobrada: débitos %, créditos %, pernas %',
            v_entry, v_debits, v_credits, v_legs
            USING ERRCODE = 'check_violation', CONSTRAINT = 'journal_entry_balanced';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER journal_entries_balanced
    AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_entry_balanced();

CREATE CONSTRAINT TRIGGER postings_balanced
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_entry_balanced();

-- Idempotência das escritas (ADR-0004, seção 3). A linha é inserida no início da transação,
-- para que uma requisição concorrente com a mesma chave espere no índice único, e completada
-- no fim, na mesma transação: fora dela, nunca se vê uma linha incompleta.
CREATE TABLE idempotency_keys (
    scope        text        NOT NULL,
    key          text        NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
    -- SHA-256 do pedido, para detectar a mesma chave com pedido diferente.
    request_hash bytea       NOT NULL CHECK (length(request_hash) = 32),
    -- Código de status gRPC do resultado (0 = OK).
    status_code  smallint    CHECK (status_code BETWEEN 0 AND 16),
    -- OK: bytes Protobuf da resposta. Recusa: bytes de um google.rpc.Status (código,
    -- mensagem e ErrorInfo), para a repetição ser idêntica.
    body         bytea,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (scope, key),
    CONSTRAINT idempotency_keys_result_complete CHECK ((status_code IS NULL) = (body IS NULL))
);

-- Privilégios do serviço, tabela a tabela. Lançamentos e pernas são append-only também por
-- privilégio: o serviço não tem UPDATE nem DELETE neles. Na conta, só o saldo muda.
GRANT SELECT, INSERT                      ON accounts         TO ledger_app;
GRANT UPDATE (balance_cents)              ON accounts         TO ledger_app;
GRANT SELECT, INSERT                      ON journal_entries  TO ledger_app;
GRANT SELECT, INSERT                      ON postings         TO ledger_app;
GRANT SELECT, INSERT                      ON idempotency_keys TO ledger_app;
GRANT UPDATE (status_code, body)          ON idempotency_keys TO ledger_app;
