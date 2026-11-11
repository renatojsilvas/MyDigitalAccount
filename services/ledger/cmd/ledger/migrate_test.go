package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"conta.local/testkit/testpg"
)

// migrateLogs roda o subcomando e devolve as linhas de log decodificadas.
func migrateLogs(t *testing.T, env map[string]string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := runMigrate(t.Context(), envFrom(env), &out); err != nil {
		t.Fatalf("runMigrate: %v\nsaída:\n%s", err, out.String())
	}
	var recs []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log fora do JSON: %q", sc.Text())
		}
		recs = append(recs, rec)
	}
	return recs
}

func TestRunMigrate(t *testing.T) {
	db := testpg.New(t)
	db.EnsureRole(t, "ledger_app", "ledger-app-test")
	env := ledgerEnv(db)

	// Primeira execução aplica; a segunda encontra o banco em dia (o job pode rodar a cada deploy).
	for i, wantApplied := range []float64{1, 0} {
		recs := migrateLogs(t, env)
		last := recs[len(recs)-1]
		if last["msg"] != "migrations em dia" || last["applied"] != wantApplied {
			t.Fatalf("execução %d: último log = %v, want %v migration(s) aplicada(s)", i+1, last, wantApplied)
		}
	}
}

func TestRunMigrateFailsWithInvalidConfig(t *testing.T) {
	var out bytes.Buffer
	if err := runMigrate(t.Context(), envFrom(map[string]string{}), &out); err == nil {
		t.Fatal("runMigrate sem configuração deveria falhar")
	}
}
