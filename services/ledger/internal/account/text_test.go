package account_test

import (
	"errors"
	"strings"
	"testing"

	"conta.local/ledger/internal/account"
)

func TestNewHolderName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "simples", in: "Maria Silva", want: "Maria Silva"},
		{name: "acentos contam como um caractere", in: strings.Repeat("é", 120), want: strings.Repeat("é", 120)},
		{name: "corta espaços das pontas", in: "  Ana  ", want: "Ana"},
		{name: "vazio", in: "", wantErr: true},
		{name: "só espaços", in: "   ", wantErr: true},
		{name: "longo demais", in: strings.Repeat("a", 121), wantErr: true},
		{name: "quebra de linha", in: "Ana\nSilva", wantErr: true},
		{name: "UTF-8 inválido", in: "Ana\xff", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := account.NewHolderName(tt.in)
			if tt.wantErr {
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "holder_name" {
					t.Fatalf("err = %v, want ValidationError em holder_name", err)
				}
				return
			}
			if err != nil || got.String() != tt.want {
				t.Fatalf("= %q, %v; want %q", got.String(), err, tt.want)
			}
		})
	}
}

func TestNewDescription(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "vazia é permitida", in: "", want: ""},
		{name: "simples", in: "Pix para João", want: "Pix para João"},
		{name: "no limite", in: strings.Repeat("x", 140), want: strings.Repeat("x", 140)},
		{name: "longa demais", in: strings.Repeat("x", 141), wantErr: true},
		{name: "tabulação", in: "a\tb", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := account.NewDescription(tt.in)
			if tt.wantErr {
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "description" {
					t.Fatalf("err = %v, want ValidationError em description", err)
				}
				return
			}
			if err != nil || got.String() != tt.want {
				t.Fatalf("= %q, %v; want %q", got.String(), err, tt.want)
			}
		})
	}
}

func TestParseID(t *testing.T) {
	if _, err := account.ParseID("0195d1f2-7c3a-7b4e-9f00-000000000001"); err != nil {
		t.Fatalf("UUID válido recusado: %v", err)
	}
	var verr *account.ValidationError
	if _, err := account.ParseID("conta-1"); !errors.As(err, &verr) || verr.Field != "account_id" {
		t.Fatalf("err = %v, want ValidationError em account_id", err)
	}
}

func TestNewIDsAreUUIDv7AndRoundTrip(t *testing.T) {
	id, err := account.NewID()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := account.ParseID(id.String())
	if err != nil || parsed != id {
		t.Fatalf("ParseID(%s) = %s, %v", id, parsed, err)
	}
	entry, err := account.NewEntryID()
	if err != nil {
		t.Fatal(err)
	}
	// O 13º caractere hexadecimal de um UUID é a versão.
	for _, s := range []string{id.String(), entry.String()} {
		if s[14] != '7' {
			t.Errorf("%s não é UUIDv7", s)
		}
	}
}
