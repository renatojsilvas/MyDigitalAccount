package account_test

import (
	"errors"
	"math"
	"testing"

	"conta.local/ledger/internal/account"
)

func TestNewAmount(t *testing.T) {
	tests := []struct {
		name    string
		cents   int64
		wantErr bool
	}{
		{name: "um centavo", cents: 1},
		{name: "máximo", cents: math.MaxInt64},
		{name: "zero", cents: 0, wantErr: true},
		{name: "negativo", cents: -1, wantErr: true},
		{name: "mínimo", cents: math.MinInt64, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := account.NewAmount(tt.cents)
			if tt.wantErr {
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "amount_cents" {
					t.Fatalf("NewAmount(%d) err = %v, want ValidationError em amount_cents", tt.cents, err)
				}
				return
			}
			if err != nil || m.Cents() != tt.cents {
				t.Fatalf("NewAmount(%d) = %d, %v", tt.cents, m.Cents(), err)
			}
		})
	}
}

func TestMoneyArithmetic(t *testing.T) {
	c := account.MoneyFromCents
	tests := []struct {
		name    string
		op      func() (account.Money, error)
		want    int64
		wantErr error
	}{
		{name: "soma", op: func() (account.Money, error) { return c(100).Add(c(250)) }, want: 350},
		{name: "subtração até zero", op: func() (account.Money, error) { return c(100).Sub(c(100)) }, want: 0},
		{name: "subtração negativa", op: func() (account.Money, error) { return c(100).Sub(c(150)) }, want: -50},
		{name: "soma no limite", op: func() (account.Money, error) { return c(math.MaxInt64 - 1).Add(c(1)) }, want: math.MaxInt64},
		{name: "soma estoura", op: func() (account.Money, error) { return c(math.MaxInt64).Add(c(1)) }, wantErr: account.ErrMoneyOverflow},
		{name: "soma negativa estoura", op: func() (account.Money, error) { return c(math.MinInt64).Add(c(-1)) }, wantErr: account.ErrMoneyOverflow},
		{name: "subtração estoura", op: func() (account.Money, error) { return c(math.MinInt64).Sub(c(1)) }, wantErr: account.ErrMoneyOverflow},
		{name: "subtração de negativo estoura", op: func() (account.Money, error) { return c(math.MaxInt64).Sub(c(-1)) }, wantErr: account.ErrMoneyOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.op()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got.Cents() != tt.want {
				t.Errorf("= %d, want %d", got.Cents(), tt.want)
			}
		})
	}
}
