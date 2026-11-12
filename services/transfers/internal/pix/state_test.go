package pix_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"conta.local/transfers/internal/pix"
)

// allowed é a tabela do ADR-0007, seção 2, escrita de novo aqui de propósito: o teste confere
// a implementação contra o documento, não contra ela mesma.
var allowed = map[[2]pix.State]bool{
	{pix.StateAccepted, pix.StateValidated}:            true,
	{pix.StateAccepted, pix.StateRejected}:             true,
	{pix.StateValidated, pix.StateDebited}:             true,
	{pix.StateValidated, pix.StateRejected}:            true,
	{pix.StateDebited, pix.StateSettling}:              true,
	{pix.StateSettling, pix.StateSettled}:              true,
	{pix.StateSettling, pix.StateReversing}:            true,
	{pix.StateSettling, pix.StatePendingConfirmation}:  true,
	{pix.StatePendingConfirmation, pix.StateSettled}:   true,
	{pix.StatePendingConfirmation, pix.StateReversing}: true,
	{pix.StateSettled, pix.StateCompleted}:             true,
	{pix.StateReversing, pix.StateReversed}:            true,
}

// Todos os 100 pares: os da tabela passam, todos os outros são recusados. Em particular, nada
// sai de um estado terminal, PENDING_CONFIRMATION nunca vai direto a REVERSED nem a COMPLETED,
// e SETTLED nunca volta (é o pivô).
func TestEveryTransitionPair(t *testing.T) {
	for _, from := range pix.States() {
		for _, to := range pix.States() {
			t.Run(fmt.Sprintf("%s->%s", from, to), func(t *testing.T) {
				if got, want := pix.CanTransition(from, to), allowed[[2]pix.State{from, to}]; got != want {
					t.Fatalf("CanTransition = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestTerminalAndStatus(t *testing.T) {
	tests := []struct {
		state    pix.State
		terminal bool
		status   pix.Status
	}{
		{pix.StateAccepted, false, pix.StatusProcessing},
		{pix.StatePendingConfirmation, false, pix.StatusProcessing},
		{pix.StateSettled, false, pix.StatusProcessing},
		{pix.StateReversing, false, pix.StatusProcessing},
		{pix.StateCompleted, true, pix.StatusCompleted},
		{pix.StateReversed, true, pix.StatusFailed},
		{pix.StateRejected, true, pix.StatusRejected},
	}
	for _, tt := range tests {
		if tt.state.Terminal() != tt.terminal || tt.state.Status() != tt.status {
			t.Errorf("%s: terminal=%v status=%v; want %v %v", tt.state, tt.state.Terminal(), tt.state.Status(), tt.terminal, tt.status)
		}
		if got, err := pix.ParseState(tt.state.String()); err != nil || got != tt.state {
			t.Errorf("ParseState(%q) = %v, %v", tt.state, got, err)
		}
	}
}

func TestTransitionValidate(t *testing.T) {
	dest, entry := uuid.New(), uuid.New()
	tests := []struct {
		name string
		tr   pix.Transition
		ok   bool
	}{
		{"validado com destino", pix.Transition{From: pix.StateAccepted, To: pix.StateValidated, DestinationAccountID: dest}, true},
		{"validado sem destino", pix.Transition{From: pix.StateAccepted, To: pix.StateValidated}, false},
		{"debitado com lançamento", pix.Transition{From: pix.StateValidated, To: pix.StateDebited, DebitEntryID: entry}, true},
		{"debitado sem lançamento", pix.Transition{From: pix.StateValidated, To: pix.StateDebited}, false},
		{"recusa com motivo de recusa", pix.Transition{From: pix.StateAccepted, To: pix.StateRejected, Reason: pix.ReasonLimitExceeded}, true},
		{"recusa sem motivo", pix.Transition{From: pix.StateAccepted, To: pix.StateRejected}, false},
		{"recusa com motivo de estorno", pix.Transition{From: pix.StateAccepted, To: pix.StateRejected, Reason: pix.ReasonSettlementRejected}, false},
		{"estorno com motivo de estorno", pix.Transition{From: pix.StateSettling, To: pix.StateReversing, Reason: pix.ReasonSettlementRejected}, true},
		{"estorno com motivo de recusa", pix.Transition{From: pix.StateSettling, To: pix.StateReversing, Reason: pix.ReasonInsufficientFunds}, false},
		{"pendente com motivo", pix.Transition{From: pix.StateSettling, To: pix.StatePendingConfirmation, Reason: pix.ReasonSettlementNotSent}, false},
		{"par fora da máquina", pix.Transition{From: pix.StatePendingConfirmation, To: pix.StateReversed, Reason: pix.ReasonSettlementRejected}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.tr.Validate()
			if tt.ok != (err == nil) {
				t.Fatalf("Validate = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, pix.ErrInvalidTransition) {
				t.Fatalf("err = %v, want ErrInvalidTransition", err)
			}
		})
	}
}

func TestKeyIsAlwaysMasked(t *testing.T) {
	k, err := pix.NewKey(pix.KeyCPF, "12345678909")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{fmt.Sprint(k), fmt.Sprintf("%v %+v %#v %s", k, k, k, k), k.Masked()} {
		if got == "" || contains(got, "12345678909") {
			t.Fatalf("chave sem máscara: %q", got)
		}
	}
	if k.Masked() != "***.456.789-**" || k.Value() != "12345678909" {
		t.Fatalf("Masked = %q, Value = %q", k.Masked(), k.Value())
	}
	for _, bad := range []string{"12345678900", "1234567890", "11111111111", "1234567890a"} {
		if _, err := pix.NewKey(pix.KeyCPF, bad); err == nil {
			t.Errorf("CPF inválido aceito: %s", bad)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCalendarDayInSaoPaulo(t *testing.T) {
	cal, err := pix.NewCalendar()
	if err != nil {
		t.Fatal(err)
	}
	// 23:59 em São Paulo (UTC-3) ainda é 02:59 UTC do dia seguinte.
	at := time.Date(2026, 11, 13, 2, 59, 0, 0, time.UTC)
	day := cal.DayOf(at)
	if day != (pix.Day{Year: 2026, Month: 11, D: 12}) {
		t.Fatalf("dia = %v, want 2026-11-12 (horário de Brasília)", day)
	}
	if next := cal.DayOf(at.Add(6 * time.Minute)); next != (pix.Day{Year: 2026, Month: 11, D: 13}) {
		t.Fatalf("00:05 = %v, want 2026-11-13", next)
	}
	if reset := cal.ResetsAt(day); !reset.Equal(time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("ResetsAt = %v, want 2026-11-13T03:00Z", reset)
	}
}

func TestNewLimitsNeverNegative(t *testing.T) {
	if l := pix.NewLimits(1_000, 1_500, time.Time{}); l.AvailableCents != 0 {
		t.Fatalf("disponível = %d, want 0", l.AvailableCents)
	}
}
