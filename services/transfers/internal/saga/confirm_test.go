package saga_test

import (
	"testing"
	"time"

	"conta.local/transfers/internal/pix"
)

// toPending leva um Pix até PENDING_CONFIRMATION (só timeouts na liquidação).
func toPending(t *testing.T, w *world) pix.Transfer {
	t.Helper()
	tr := w.pix(t, "k", 30_000, destCPF)
	w.drain(t)
	if got, _ := w.get(t, tr.ID); got.State != pix.StatePendingConfirmation || got.PendingSince.IsZero() {
		t.Fatalf("estado %v (pendente desde %v), want pending_confirmation", got.State, got.PendingSince)
	}
	return tr
}

// confirm faz rodadas do worker de confirmação e depois do runner, n vezes.
func (w *world) confirm(t *testing.T, n int) {
	t.Helper()
	for range n {
		time.Sleep(3 * time.Millisecond) // o intervalo entre consultas é de 1 a 2 ms nos testes
		if _, err := w.runner.ConfirmOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
		w.drain(t)
	}
}

func TestConfirmation(t *testing.T) {
	tests := []struct {
		name       string
		status     []queryResult
		rounds     int
		wantState  pix.State
		wantReason pix.Reason
		wantCredit int
		wantRev    int
	}{
		// Timeout com liquidação feita: a consulta mostra liquidado, e a saga conclui.
		{"timeout com liquidação feita", []queryResult{querySettled}, 1, pix.StateCompleted, 0, 1, 0},
		// O SPI recusou: só agora, com a recusa confirmada, o débito é estornado.
		{"recusa confirmada na consulta", []queryResult{queryRejected}, 1, pix.StateReversed, pix.ReasonSettlementRejected, 0, 1},
		// Timeout sem liquidação: "não encontrado" não prova nada; continua pendente, com o débito.
		{"timeout sem liquidação", []queryResult{queryNotFound}, 3, pix.StatePendingConfirmation, 0, 0, 0},
		{"consulta falhando", []queryResult{queryFailed}, 3, pix.StatePendingConfirmation, 0, 0, 0},
		{"não encontrado e depois liquidado (em trânsito)", []queryResult{queryNotFound, queryNotFound, querySettled}, 3,
			pix.StateCompleted, 0, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, spiTimeout)
			w.spi.status = tt.status
			tr := toPending(t, w)
			w.confirm(t, tt.rounds)

			got, _ := w.get(t, tr.ID)
			if got.State != tt.wantState || got.Reason != tt.wantReason {
				t.Fatalf("estado %v (%v), want %v (%v)", got.State, got.Reason, tt.wantState, tt.wantReason)
			}
			if w.ledger.credits != tt.wantCredit || w.ledger.reversals != tt.wantRev {
				t.Fatalf("créditos %d, estornos %d; want %d e %d", w.ledger.credits, w.ledger.reversals, tt.wantCredit, tt.wantRev)
			}
			if tt.wantState == pix.StatePendingConfirmation {
				if got.ConfirmationAttempts != tt.rounds || w.ledger.balance[w.source] != 70_000 || got.ConfirmationOverdue {
					t.Fatalf("pendente: %d consultas registradas, saldo %d, overdue=%v",
						got.ConfirmationAttempts, w.ledger.balance[w.source], got.ConfirmationOverdue)
				}
			}
		})
	}
}

// Prazo máximo estourado: continua pendente (nunca estorna nem conclui), marcado overdue, e o
// aviso sai uma vez só; as consultas continuam, no intervalo máximo.
func TestDeadlineExceededStaysPending(t *testing.T) {
	w := newWorldWith(t, time.Millisecond, spiTimeout)
	w.spi.status = []queryResult{queryNotFound}
	tr := toPending(t, w)
	w.confirm(t, 3)

	got, _ := w.get(t, tr.ID)
	if got.State != pix.StatePendingConfirmation || !got.ConfirmationOverdue || got.State.Status() != pix.StatusProcessing {
		t.Fatalf("estado %v, overdue=%v; want pendente e overdue", got.State, got.ConfirmationOverdue)
	}
	if w.ledger.reversals != 0 || w.ledger.credits != 0 || w.ledger.balance[w.source] != 70_000 {
		t.Fatalf("prazo estourado mexeu no dinheiro: estornos %d, créditos %d, saldo %d",
			w.ledger.reversals, w.ledger.credits, w.ledger.balance[w.source])
	}
	if w.rec.overdue != 1 || w.spi.queries != 3 {
		t.Fatalf("avisos de prazo %d (want 1), consultas %d (want 3)", w.rec.overdue, w.spi.queries)
	}
	stats, err := w.store.PendingStats(t.Context())
	if err != nil || stats.Pending != 1 || stats.Overdue != 1 || stats.OldestAge <= 0 {
		t.Fatalf("PendingStats = %+v, %v", stats, err)
	}
}

// Reinício do processo com Pix pendente: o worker lê os pendentes do banco, então uma instância
// nova (com memória vazia) retoma e conclui.
func TestRestartWithPendingPix(t *testing.T) {
	w := newWorld(t, spiTimeout)
	tr := toPending(t, w)

	// "Reinício": o runner antigo some; um novo, sobre o mesmo banco, começa do zero.
	w.spi.status = []queryResult{querySettled}
	w.runner = w.newRunner(time.Hour)
	w.confirm(t, 1)

	got, h := w.get(t, tr.ID)
	if got.State != pix.StateCompleted || w.ledger.debits != 1 || w.ledger.credits != 1 {
		t.Fatalf("depois do reinício: estado %v, débitos %d, créditos %d", got.State, w.ledger.debits, w.ledger.credits)
	}
	if last := h[len(h)-1]; last.From != pix.StateSettled || last.To != pix.StateCompleted {
		t.Fatalf("última transição %v -> %v", last.From, last.To)
	}
}

// Queda do processo no meio da saga: o runner A tomou o Pix e morreu sem soltar o lease. Até o
// lease expirar, ninguém mexe; depois, outra instância retoma do estado gravado e conclui.
func TestCrashedRunnerLeaseExpiresAndAnotherResumes(t *testing.T) {
	w := newWorld(t)
	tr := w.pix(t, "k", 30_000, destCPF)
	jobs, err := w.store.Claim(t.Context(), "runner-a", 200*time.Millisecond, []pix.State{pix.StateAccepted}, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("A não tomou o Pix: %v, %d", err, len(jobs))
	}
	// A morreu aqui. B tenta já: o lease de A ainda vale.
	if n, err := w.runner.RunOnce(t.Context()); err != nil || n != 0 {
		t.Fatalf("B tomou um Pix com lease ativo: %d, %v", n, err)
	}
	time.Sleep(250 * time.Millisecond)
	w.drain(t)
	if got, _ := w.get(t, tr.ID); got.State != pix.StateCompleted || w.ledger.debits != 1 {
		t.Fatalf("depois da retomada: estado %v, débitos %d", got.State, w.ledger.debits)
	}
}
