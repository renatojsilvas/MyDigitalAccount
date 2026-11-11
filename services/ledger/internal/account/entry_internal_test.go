package account

import (
	"errors"
	"math"
	"testing"
)

// A invariante da partida dobrada é testada direto em newEntry, porque os métodos públicos
// só montam lançamentos válidos e não conseguem exercitar os casos de recusa.
func TestNewEntryInvariant(t *testing.T) {
	a, b, c := ID{15: 0xa}, ID{15: 0xb}, ID{15: 0xc}
	m := MoneyFromCents
	tests := []struct {
		name    string
		legs    []Leg
		wantErr bool
	}{
		{name: "duas pernas balanceadas", legs: []Leg{{a, DirectionDebit, m(100)}, {b, DirectionCredit, m(100)}}},
		{name: "três pernas balanceadas", legs: []Leg{{a, DirectionDebit, m(100)}, {b, DirectionCredit, m(60)}, {c, DirectionCredit, m(40)}}},
		{name: "uma perna", legs: []Leg{{a, DirectionDebit, m(100)}}, wantErr: true},
		{name: "nenhuma perna", wantErr: true},
		{name: "desbalanceado", legs: []Leg{{a, DirectionDebit, m(100)}, {b, DirectionCredit, m(99)}}, wantErr: true},
		{name: "só débitos", legs: []Leg{{a, DirectionDebit, m(100)}, {b, DirectionDebit, m(100)}}, wantErr: true},
		{name: "valor zero", legs: []Leg{{a, DirectionDebit, m(0)}, {b, DirectionCredit, m(0)}}, wantErr: true},
		{name: "valor negativo", legs: []Leg{{a, DirectionDebit, m(-1)}, {b, DirectionCredit, m(-1)}}, wantErr: true},
		{name: "mesma conta nos dois lados", legs: []Leg{{a, DirectionDebit, m(100)}, {a, DirectionCredit, m(100)}}, wantErr: true},
		{name: "sentido desconhecido", legs: []Leg{{a, Direction(0), m(100)}, {b, DirectionCredit, m(100)}}, wantErr: true},
		{name: "soma estoura", legs: []Leg{{a, DirectionDebit, m(math.MaxInt64)}, {b, DirectionDebit, m(1)}, {c, DirectionCredit, m(1)}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newEntry(Movement{}, Posting{}, tt.legs...)
			if tt.wantErr != errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
