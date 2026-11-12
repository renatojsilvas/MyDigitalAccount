package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/ledger/internal/account"
	"conta.local/ledger/internal/platform/postgres/pgtest"
	"conta.local/ledger/migrations"
	"conta.local/platform/pg"
	"conta.local/testkit/testpg"
)

const (
	codeCheckViolation        = "23514"
	codeInsufficientPrivilege = "42501"
)

// Os testes deste pacote conferem o schema do ledger (migrations, constraints, privilégios);
// abrir o pool e aplicar migrations é de conta.local/platform/pg.

func fromTestDB(db *testpg.Database) pg.Config {
	return pg.Config{
		Host: db.Config.Host, Port: db.Config.Port, User: db.Config.User,
		Password: db.Config.Password, Database: db.Name,
	}
}

// migrated devolve um pool como ledger_app num banco já migrado.
func migrated(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_, pool := pgtest.Migrated(t, 0)
	return pool
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
	db.EnsureRole(t, pgtest.AppRole, pgtest.AppPassword)
	cfg := fromTestDB(db)

	first, err := pg.Migrate(t.Context(), cfg, migrations.FS)
	if err != nil {
		t.Fatalf("primeira execução: %v", err)
	}
	if len(first) == 0 || first[0].Version != 1 {
		t.Fatalf("primeira execução aplicou %+v, want a partir da versão 1", first)
	}
	second, err := pg.Migrate(t.Context(), cfg, migrations.FS)
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
		entry = `INSERT INTO journal_entries (id, description, posted_at, reason) VALUES ('` + entry1 + `', 't', now(), 'external')`
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
		`INSERT INTO journal_entries (id, description, posted_at, reason) VALUES ('`+entry1+`', 't', now(), 'external')`,
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

// TestMigration2BackfillsReason aplica só a 00001, grava lançamentos no formato antigo e
// então aplica o resto: o motivo sai da contrapartida (ADR-0009, seção 2).
func TestMigration2BackfillsReason(t *testing.T) {
	db := testpg.New(t)
	db.EnsureRole(t, pgtest.AppRole, pgtest.AppPassword)
	cfg := fromTestDB(db)

	first, err := fs.ReadFile(migrations.FS, "00001_ledger_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Migrate(t.Context(), cfg, fstest.MapFS{"00001_ledger_schema.sql": {Data: first}}); err != nil {
		t.Fatalf("aplicando só a 00001: %v", err)
	}

	admin := db.Pool // superusuário de teste: grava como faria o serviço antes da 00002
	const (
		opening  = "0195d1f2-0000-7000-8000-0000000000f1"
		external = "0195d1f2-0000-7000-8000-0000000000f2"
	)
	if err := inTx(t.Context(), admin,
		`INSERT INTO accounts (id, kind, holder_name, holder_document, status, balance_cents, created_at)
			VALUES ('`+customerA+`', 'customer', 'Teste', '11144477735', 'active', 60, now())`,
		`INSERT INTO journal_entries (id, description, posted_at) VALUES ('`+opening+`', 'saldo', now())`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents) VALUES
			('`+opening+`', '00000000-0000-0000-0000-000000000001', 'debit', 100)`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents) VALUES
			('`+opening+`', '`+customerA+`', 'credit', 100, 100)`,
		`INSERT INTO journal_entries (id, description, posted_at) VALUES ('`+external+`', 'pix', now())`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents) VALUES
			('`+external+`', '`+customerA+`', 'debit', 40, 60)`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents) VALUES
			('`+external+`', '`+settlement+`', 'credit', 40)`,
	); err != nil {
		t.Fatalf("gravando lançamentos antigos: %v", err)
	}

	applied, err := pg.Migrate(t.Context(), cfg, migrations.FS)
	if err != nil {
		t.Fatalf("aplicando o resto: %v", err)
	}
	if len(applied) == 0 || applied[0].Version != 2 {
		t.Fatalf("aplicou %+v, want a partir da versão 2", applied)
	}

	got := map[string]string{}
	rows, err := admin.Query(t.Context(), "SELECT id::text, reason FROM journal_entries")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			t.Fatal(err)
		}
		got[id] = reason
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got[opening] != "opening" || got[external] != "external" || len(got) != 2 {
		t.Fatalf("motivos = %v, want opening e external", got)
	}
}

func TestDatabaseGuardsReversals(t *testing.T) {
	const (
		reversal1 = "0195d1f2-0000-7000-8000-0000000000e2"
		reversal2 = "0195d1f2-0000-7000-8000-0000000000e3"
	)
	original := []string{
		`INSERT INTO journal_entries (id, description, posted_at, reason, external_reference)
			VALUES ('` + entry1 + `', 'pix', now(), 'external', 'transfer-1')`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents)
			VALUES ('` + entry1 + `', '` + customerA + `', 'debit', 100, 0)`,
		`INSERT INTO postings (entry_id, account_id, direction, amount_cents)
			VALUES ('` + entry1 + `', '` + settlement + `', 'credit', 100)`,
	}
	reversal := func(id, target string) []string {
		return []string{
			`INSERT INTO journal_entries (id, description, posted_at, reason, reverses_entry_id)
				VALUES ('` + id + `', 'estorno', now(), 'reversal', ` + target + `)`,
			`INSERT INTO postings (entry_id, account_id, direction, amount_cents)
				VALUES ('` + id + `', '` + settlement + `', 'debit', 100)`,
			`INSERT INTO postings (entry_id, account_id, direction, amount_cents, balance_after_cents)
				VALUES ('` + id + `', '` + customerA + `', 'credit', 100, 100)`,
		}
	}

	tests := []struct {
		name       string
		stmts      [][]string // cada item numa transação; só a última pode falhar
		constraint string     // vazio = deve passar
		code       string
	}{
		{name: "estorno que aponta o original", stmts: [][]string{original, reversal(reversal1, "'"+entry1+"'")}},
		{
			name:  "segundo estorno do mesmo lançamento",
			stmts: [][]string{original, reversal(reversal1, "'"+entry1+"'"), reversal(reversal2, "'"+entry1+"'")},
			code:  "23505", constraint: "journal_entries_reverses_entry_id_key",
		},
		{
			name:  "estorno sem apontar o original",
			stmts: [][]string{original, reversal(reversal1, "NULL")},
			code:  codeCheckViolation, constraint: "journal_entries_reversal_target",
		},
		{
			name: "lançamento comum apontando um original",
			stmts: [][]string{original, {`INSERT INTO journal_entries (id, description, posted_at, reason, reverses_entry_id)
				VALUES ('` + reversal1 + `', 'x', now(), 'external', '` + entry1 + `')`}},
			code: codeCheckViolation, constraint: "journal_entries_reversal_target",
		},
		{
			name: "motivo desconhecido",
			stmts: [][]string{{`INSERT INTO journal_entries (id, description, posted_at, reason)
				VALUES ('` + entry1 + `', 'x', now(), 'gift')`}},
			code: codeCheckViolation, constraint: "journal_entries_reason",
		},
		{
			name: "referência externa com espaço",
			stmts: [][]string{{`INSERT INTO journal_entries (id, description, posted_at, reason, external_reference)
				VALUES ('` + entry1 + `', 'x', now(), 'external', 'a b')`}},
			code: codeCheckViolation, constraint: "journal_entries_external_reference_check",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := migrated(t)
			insertCustomer(t, app, 100)
			var err error
			for i, stmts := range tt.stmts {
				err = inTx(t.Context(), app, stmts...)
				if err != nil && i < len(tt.stmts)-1 {
					t.Fatalf("transação %d: %v", i, err)
				}
			}
			if tt.constraint == "" {
				if err != nil {
					t.Fatalf("err = %v, want commit", err)
				}
				return
			}
			pgErr := pgError(t, err)
			if pgErr.Code != tt.code || pgErr.ConstraintName != tt.constraint {
				t.Fatalf("err = %s (%s), want %s em %s", pgErr.Code, pgErr.ConstraintName, tt.code, tt.constraint)
			}
		})
	}
}
