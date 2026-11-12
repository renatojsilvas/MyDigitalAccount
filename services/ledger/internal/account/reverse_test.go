package account_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"conta.local/ledger/internal/account"
)

func mustRef(t *testing.T, s string) account.ExternalReference {
	t.Helper()
	r, err := account.NewExternalReference(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// pixDebit faz o débito de um Pix com a referência "transfer-1", como o transfers fará.
func (f fixture) pixDebit(t *testing.T, id account.ID, cents int64) account.Posting {
	t.Helper()
	cmd := debitCmd(t, newKey(t), id, cents)
	cmd.Reference = mustRef(t, "transfer-1")
	p, err := f.svc.Debit(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}
	return p
}

func reverseCmd(t *testing.T, entry account.EntryID, ref string) account.ReverseCommand {
	t.Helper()
	desc, _ := account.NewDescription("Estorno do Pix")
	return account.ReverseCommand{Key: newKey(t), EntryID: entry, Description: desc, Reference: mustRef(t, ref)}
}

func TestReverseRestoresBalanceAndPointsToOriginal(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	debit := f.pixDebit(t, a.ID(), 300)

	p, err := f.svc.Reverse(t.Context(), reverseCmd(t, debit.EntryID, "transfer-1"))
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if p.Direction != account.DirectionCredit || p.Amount.Cents() != 300 || p.BalanceAfter.Cents() != 1_000 ||
		p.Reason != account.ReasonReversal || p.ReversesEntryID != debit.EntryID || p.Reference.String() != "transfer-1" {
		t.Fatalf("estorno = %+v", p)
	}
	if got := f.balance(t, a.ID()); got != 1_000 {
		t.Fatalf("saldo %d, want 1000", got)
	}
	f.assertLedgerConsistent(t, a.ID())

	// O extrato traz o motivo e a referência de cada lançamento, lidos do banco.
	lines := f.allLines(t, a.ID())
	if len(lines) != 3 {
		t.Fatalf("extrato com %d linhas, want 3", len(lines))
	}
	wantReasons := []account.EntryReason{account.ReasonOpening, account.ReasonExternal, account.ReasonReversal}
	for i, l := range lines {
		if l.Posting.Reason != wantReasons[i] {
			t.Fatalf("linha %d: motivo %v, want %v", i, l.Posting.Reason, wantReasons[i])
		}
	}
	if lines[1].Posting.Reference.String() != "transfer-1" || lines[2].Posting.ReversesEntryID != debit.EntryID {
		t.Fatalf("referências no extrato: %+v / %+v", lines[1].Posting, lines[2].Posting)
	}
	// Na conta settlement, o débito e o estorno se anulam.
	net, err := f.store.NetPostings(t.Context(), account.SettlementAccountID)
	if err != nil || net.Cents() != 0 {
		t.Fatalf("settlement = %d, %v; want 0", net.Cents(), err)
	}
}

// Camada 2 do ADR-0009: com outra chave, o segundo estorno é recusado, e a recusa é gravada.
func TestReverseTwiceIsRejected(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	debit := f.pixDebit(t, a.ID(), 300)
	if _, err := f.svc.Reverse(t.Context(), reverseCmd(t, debit.EntryID, "transfer-1")); err != nil {
		t.Fatal(err)
	}

	second := reverseCmd(t, debit.EntryID, "transfer-1")
	if _, err := f.svc.Reverse(t.Context(), second); !errors.Is(err, account.ErrEntryAlreadyReversed) {
		t.Fatalf("err = %v, want ErrEntryAlreadyReversed", err)
	}
	var replay replayedRejection
	if _, err := f.svc.Reverse(t.Context(), second); !errors.As(err, &replay) || replay.Code != 9 {
		t.Fatalf("repetição = %v, want a recusa gravada", err)
	}
	if got := f.balance(t, a.ID()); got != 1_000 {
		t.Fatalf("saldo %d, want 1000 (um único estorno)", got)
	}
	f.assertLedgerConsistent(t, a.ID())
}

// Estornos concorrentes do mesmo débito, cada um com a sua chave: o lock da conta serializa,
// e exatamente um passa.
func TestConcurrentReversalsPostOnce(t *testing.T) {
	f := newFixture(t, 5*time.Second)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	debit := f.pixDebit(t, a.ID(), 300)

	const n = 10
	errs := make([]error, n)
	cmds := make([]account.ReverseCommand, n)
	for i := range cmds {
		cmds[i] = reverseCmd(t, debit.EntryID, "transfer-1")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			_, errs[i] = f.svc.Reverse(context.Background(), cmds[i])
		})
	}
	close(start)
	wg.Wait()

	var ok int
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, account.ErrEntryAlreadyReversed):
		default:
			t.Fatalf("estorno %d: erro inesperado: %v", i, err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d estornos aceitos, want 1", ok)
	}
	if got := f.balance(t, a.ID()); got != 1_000 {
		t.Fatalf("saldo %d, want 1000", got)
	}
	f.assertLedgerConsistent(t, a.ID())
}

func TestReverseRejections(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)
	opening := f.allLines(t, a.ID())[0].Posting.EntryID

	external, err := f.svc.Credit(t.Context(), account.CreditCommand{
		Key: newKey(t), AccountID: a.ID(), Amount: mustAmount(t, 50), Reason: account.CreditExternal,
	})
	if err != nil {
		t.Fatal(err)
	}
	debit := f.pixDebit(t, a.ID(), 100)
	reversal, err := f.svc.Reverse(t.Context(), reverseCmd(t, debit.EntryID, "transfer-1"))
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := account.NewEntryID()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		entry account.EntryID
		want  error
	}{
		{"saldo inicial do seed", opening, account.ErrEntryNotReversible},
		{"crédito externo", external.EntryID, account.ErrEntryNotReversible},
		{"estorno de um estorno", reversal.EntryID, account.ErrEntryNotReversible},
		{"lançamento inexistente", unknown, account.ErrEntryNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.balance(t, a.ID())
			if _, err := f.svc.Reverse(t.Context(), reverseCmd(t, tt.entry, "x")); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if got := f.balance(t, a.ID()); got != before {
				t.Fatalf("saldo mudou de %d para %d numa recusa", before, got)
			}
		})
	}
	f.assertLedgerConsistent(t, a.ID())
}

// A referência faz parte do pedido: a mesma chave com outra referência é outro pedido.
func TestReferenceIsPartOfTheRequest(t *testing.T) {
	f := newFixture(t, 0)
	a := f.open(t)
	f.fund(t, a.ID(), 1_000)

	cmd := debitCmd(t, newKey(t), a.ID(), 100)
	cmd.Reference = mustRef(t, "transfer-1")
	first, err := f.svc.Debit(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.Debit(t.Context(), cmd)
	if err != nil || again.EntryID != first.EntryID || again.Reference != first.Reference {
		t.Fatalf("repetição = %+v, %v; want o resultado original", again, err)
	}
	cmd.Reference = mustRef(t, "transfer-2")
	if _, err := f.svc.Debit(t.Context(), cmd); !errors.Is(err, account.ErrIdempotencyKeyReused) {
		t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
	}
}
