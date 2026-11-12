package account_test

import (
	"errors"
	"strings"
	"testing"

	"conta.local/ledger/internal/account"
)

func TestNewExternalReference(t *testing.T) {
	tests := []struct {
		name, in string
		ok       bool
	}{
		{"vazia é sem referência", "", true},
		{"uuid", "0195d1f2-0000-7000-8000-00000000000a", true},
		{"64 caracteres", strings.Repeat("a", 64), true},
		{"65 caracteres", strings.Repeat("a", 65), false},
		{"espaço", "a b", false},
		{"acento", "transferência", false},
		{"controle", "a\nb", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := account.NewExternalReference(tt.in)
			if !tt.ok {
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "external_reference" {
					t.Fatalf("err = %v, want ValidationError em external_reference", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if r.String() != tt.in || r.IsZero() != (tt.in == "") {
				t.Fatalf("referência = %q (zero=%v)", r.String(), r.IsZero())
			}
		})
	}
}
