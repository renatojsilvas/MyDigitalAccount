package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/platform/postgres"
	"conta.local/ledger/migrations"
	"conta.local/testkit/testpg"
)

// Papel do serviço na instância de teste. A senha é a mesma em todos os testes porque o
// papel é compartilhado pelo cluster (testpg.EnsureRole).
const (
	appRole     = "ledger_app"
	appPassword = "ledger-app-test" //nolint:gosec // credencial só da instância de teste
)

const (
	codeCheckViolation        = "23514"
	codeInsufficientPrivilege = "42501"
)

// migrated cria um banco, aplica as migrations como dono e devolve um pool conectado como
// ledger_app, o papel restrito do serviço.
func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	db := testpg.New(t)
	db.EnsureRole(t, appRole, appPassword)
	if _, err := postgres.Migrate(t.Context(), fromTestDB(db), migrations.FS); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db.ConnectAs(t, appRole, appPassword)
}

func pgError(t *testing.T, err error) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("err = %v, want erro do Postgres", err)
	}
	return pgErr
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testpg.New(t)
	db.EnsureRole(t, appRole, appPassword)
	cfg := fromTestDB(db)

	first, err := postgres.Migrate(t.Context(), cfg, migrations.FS)
	if err != nil {
		t.Fatalf("primeira execução: %v", err)
	}
	if len(first) == 0 || first[0].Version != 1 {
		t.Fatalf("primeira execução aplicou %+v, want a partir da versão 1", first)
	}
	second, err := postgres.Migrate(t.Context(), cfg, migrations.FS)
	if err != nil || len(second) != 0 {
		t.Fatalf("segunda execução = %+v, %v; want nada a aplicar", second, err)
	}
}

func TestInternalAccountsMatchDomain(t *testing.T) {
	app := migrated(t)
	want := map[string]string{
		"opening_balance": account.OpeningBalanceAccountID.String(),
		"settlement":      account.SettlementAccountID.String(),
	}
	rows, err := app.Query(t.Context(), "SELECT code, id::text FROM accounts WHERE kind = 'internal'")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var code, id string
		if err := rows.Scan(&code, &id); err != nil {
			t.Fatal(err)
		}
		got[code] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) || got["opening_balance"] != want["opening_balance"] || got["settlement"] != want["settlement"] {
		t.Fatalf("contas internas = %v, want %v", got, want)
	}
}

const (
	customerA  = "0195d1f2-0000-7000-8000-00000000000a"
	customerB  = "0195d1f2-0000-7000-8000-00000000000b"
	settlement = "00000000-0000-0000-0000-000000000002"
	entry1     = "0195d1f2-0000-7000-8000-0000000000e1"
)

// insertCustomer cria a conta customerA, com o CPF 111.444.777-35.
func insertCustomer(t *testing.T, app *pgxpool.Pool, balance int64) {
	t.Helper()
	_, err := app.Exec(t.Context(), `INSERT INTO accounts
		(id, kind, holder_name, holder_document, status, balance_cents, created_at)
		VALUES ($1, 'customer', 'Teste', '11144477735', 'active', $2, now())`, customerA, balance)
	if err != nil {
		t.Fatalf("inserindo conta: %v", err)
	}
}

// inTx roda os comandos numa transação e devolve o erro do primeiro comando que falhar ou,
// se todos passarem, o erro do COMMIT (onde as constraints adiadas são conferidas).
func inTx(ctx context.Context, app *pgxpool.Pool, stmts ...string) error {
	tx, err := app.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func TestDatabaseEnforcesDoubleEntry(t *testing.T) {
	const (
		entry = `INSERT INTO journal_entries (id, description, posted_at) VALUES ('` + entry1 + `', 't', now())`
		debit = `INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents)
			VALUES ('` + entry1 + `', '` + customerA + `', 'debit', 100, 0)`
		credit = `INSERT INTO postings (entry_id, account_id, direction, amount_cents)
			VALUES ('` + entry1 + `', '` + settlement + `', 'credit', 100)`
		creditShort = `INSERT INTO postings (entry_id, account_id, direction, amount_cents)
			VALUES ('` + entry1 + `', '` + settlement + `', 'credit', 99)`
	)
	tests := []struct {
		name  string
		stmts []string
		ok    bool
	}{
		{name: "balanceado", stmts: []string{entry, debit, credit}, ok: true},
		{name: "pernas antes do lançamento na mesma transação", stmts: []string{entry, credit, debit}, ok: true},
		{name: "desbalanceado", stmts: []string{entry, debit, creditShort}},
		{name: "uma perna", stmts: []string{entry, debit}},
		{name: "sem pernas", stmts: []string{entry}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := migrated(t)
			insertCustomer(t, app, 100)

			err := inTx(t.Context(), app, tt.stmts...)
			if tt.ok {
				if err != nil {
					t.Fatalf("err = %v, want commit", err)
				}
				return
			}
			pgErr := pgError(t, err)
			if pgErr.Code != codeCheckViolation || pgErr.ConstraintName != "journal_entry_balanced" {
				t.Fatalf("err = %s (%s), want check_violation em journal_entry_balanced", pgErr.Code, pgErr.ConstraintName)
			}
		})
	}

	t.Run("perna acrescentada depois a um lançamento gravado", func(t *testing.T) {
		app := migrated(t)
		insertCustomer(t, app, 100)
		if err := inTx(t.Context(), app, entry, debit, credit); err != nil {
			t.Fatal(err)
		}
		extra := `INSERT INTO postings (entry_id, account_id, direction, amount_cents)
			VALUES ('` + entry1 + `', '00000000-0000-0000-0000-000000000001', 'credit', 1)`
		pgErr := pgError(t, inTx(t.Context(), app, extra))
		if pgErr.ConstraintName != "journal_entry_balanced" {
			t.Fatalf("constraint = %q, want journal_entry_balanced", pgErr.ConstraintName)
		}
	})
}

func TestDatabaseRejectsNegativeBalance(t *testing.T) {
	app := migrated(t)
	insertCustomer(t, app, 100)

	if _, err := app.Exec(t.Context(), "UPDATE accounts SET balance_cents = 0 WHERE id = $1", customerA); err != nil {
		t.Fatalf("zerar o saldo deve ser permitido: %v", err)
	}
	_, err := app.Exec(t.Context(), "UPDATE accounts SET balance_cents = -1 WHERE id = $1", customerA)
	pgErr := pgError(t, err)
	if pgErr.Code != codeCheckViolation || pgErr.ConstraintName != "accounts_balance_non_negative" {
		t.Fatalf("err = %s (%s), want accounts_balance_non_negative", pgErr.Code, pgErr.ConstraintName)
	}
}

func TestOneAccountPerDocument(t *testing.T) {
	app := migrated(t)
	insertCustomer(t, app, 0)

	// O caminho do OpenAccount: ON CONFLICT DO NOTHING não aborta a transação, e "nenhuma
	// linha devolvida" significa CPF já cadastrado (ADR-0004, seção 4).
	tx, err := app.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	var id string
	err = tx.QueryRow(t.Context(), `INSERT INTO accounts
		(id, kind, holder_name, holder_document, status, balance_cents, created_at)
		VALUES ($1, 'customer', 'Outro', '11144477735', 'active', 0, now())
		ON CONFLICT (holder_document) DO NOTHING RETURNING id::text`, customerB).Scan(&id)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v, want nenhuma linha (CPF duplicado)", err)
	}
	// A transação continua utilizável: é aqui que a recusa seria gravada.
	if _, err := tx.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatalf("transação abortada depois do conflito: %v", err)
	}
}

func TestServiceRolePrivileges(t *testing.T) {
	app := migrated(t)
	insertCustomer(t, app, 100)
	if err := inTx(t.Context(), app,
		`INSERT INTO journal_entries (id, description, posted_at) VALUES ('`+entry1+`', 't', now())`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents)
			VALUES ('`+entry1+`', '`+customerA+`', 'debit', 100, 0)`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents)
			VALUES ('`+entry1+`', '`+settlement+`', 'credit', 100)`,
	); err != nil {
		t.Fatalf("o serviço deve conseguir lançar: %v", err)
	}

	denied := []struct{ name, sql string }{
		{"alterar perna", "UPDATE postings SET amount_cents = 1"},
		{"apagar perna", "DELETE FROM postings"},
		{"alterar lançamento", "UPDATE journal_entries SET description = 'x'"},
		{"apagar lançamento", "DELETE FROM journal_entries"},
		{"truncar pernas", "TRUNCATE postings"},
		{"alterar titular", "UPDATE accounts SET holder_name = 'x'"},
		{"alterar CPF", "UPDATE accounts SET holder_document = '52998224725'"},
		{"apagar conta", "DELETE FROM accounts"},
		{"alterar hash de idempotência", "UPDATE idempotency_keys SET request_hash = ''"},
		{"apagar chave de idempotência", "DELETE FROM idempotency_keys"},
		{"criar tabela (DDL)", "CREATE TABLE x (id int)"},
		{"remover tabela (DDL)", "DROP TABLE postings"},
		{"desligar a trigger", "ALTER TABLE postings DISABLE TRIGGER postings_balanced"},
	}
	for _, tt := range denied {
		t.Run(tt.name, func(t *testing.T) {
			_, err := app.Exec(t.Context(), tt.sql)
			pgErr := pgError(t, err)
			// DDL em tabela alheia também responde 42501 ("must be owner").
			if pgErr.Code != codeInsufficientPrivilege {
				t.Fatalf("err = %s %s, want 42501 (sem privilégio)", pgErr.Code, pgErr.Message)
			}
		})
	}
}

func TestIdempotencyRowIsCompleteOrPending(t *testing.T) {
	app := migrated(t)
	hash := make([]byte, 32)
	ctx := t.Context()

	// Inserida pendente no início da transação, completada no fim.
	if _, err := app.Exec(ctx, `INSERT INTO idempotency_keys (scope, key, request_hash) VALUES ('debit', 'k1', $1)`, hash); err != nil {
		t.Fatalf("inserir pendente: %v", err)
	}
	if _, err := app.Exec(ctx, `UPDATE idempotency_keys SET status_code = 0, body = '\x0a'::bytea WHERE scope = 'debit' AND key = 'k1'`); err != nil {
		t.Fatalf("completar: %v", err)
	}

	tests := []struct {
		name, sql string
		hash      []byte
	}{
		{"status sem corpo", `INSERT INTO idempotency_keys (scope, key, request_hash, status_code) VALUES ('debit', 'k2', $1, 0)`, hash},
		{"chave vazia", `INSERT INTO idempotency_keys (scope, key, request_hash) VALUES ('debit', '', $1)`, hash},
		{"hash curto", `INSERT INTO idempotency_keys (scope, key, request_hash) VALUES ('debit', 'k3', $1)`, hash[:8]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := app.Exec(ctx, tt.sql, tt.hash)
			if pgErr := pgError(t, err); pgErr.Code != codeCheckViolation {
				t.Fatalf("err = %s %s, want check_violation", pgErr.Code, pgErr.Message)
			}
		})
	}
}
