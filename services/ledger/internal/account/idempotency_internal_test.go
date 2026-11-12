package account

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNewIdempotencyKey(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "uuid", in: "0195d1f2-7c3a-7b4e-9f00-000000000001"},
		{name: "composta pelo cliente", in: "pix:123:debit"},
		{name: "no limite", in: strings.Repeat("k", 128)},
		{name: "vazia", in: "", wantErr: true},
		{name: "longa demais", in: strings.Repeat("k", 129), wantErr: true},
		{name: "com espaço", in: "a b", wantErr: true},
		{name: "não ASCII", in: "chave-ç", wantErr: true},
		{name: "controle", in: "a\nb", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := NewIdempotencyKey(tt.in)
			var verr *ValidationError
			if tt.wantErr != errors.As(err, &verr) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && k.String() != tt.in {
				t.Fatalf("String() = %q", k.String())
			}
		})
	}
}

func TestRequestHashSeparatesFields(t *testing.T) {
	if bytes.Equal(requestHash("ab", "c"), requestHash("a", "bc")) {
		t.Fatal(`("ab","c") e ("a","bc") não podem ter o mesmo hash`)
	}
	if bytes.Equal(requestHash("a"), requestHash("a", "")) {
		t.Fatal("um campo vazio a mais precisa mudar o hash")
	}
	if !bytes.Equal(requestHash("x", "1"), requestHash("x", "1")) || len(requestHash("x")) != 32 {
		t.Fatal("o hash deve ser determinístico e ter 32 bytes")
	}
}

func TestIsBusinessRejection(t *testing.T) {
	for _, err := range []error{ErrInsufficientFunds, ErrAccountNotFound, ErrAccountAlreadyExists} {
		if !isBusinessRejection(err) {
			t.Errorf("%v deveria ser recusa de negócio", err)
		}
	}
	for _, err := range []error{ErrIdempotencyKeyReused, ErrMoneyOverflow, ErrInvalidEntry, errors.New("rede")} {
		if isBusinessRejection(err) {
			t.Errorf("%v não é recusa de negócio: não pode ser gravado", err)
		}
	}
}

// Sem referência, o hash é o mesmo de antes de o campo existir (ADR-0009, seção 3).
func TestHashWithoutReferenceIsUnchanged(t *testing.T) {
	fields := []string{"conta", "100", "1", "desc"}
	if !bytes.Equal(requestHash(withReference(ExternalReference{}, fields...)...), requestHash(fields...)) {
		t.Fatal("hash sem referência mudou")
	}
	ref := ExternalReference{value: "transfer-1"}
	if bytes.Equal(requestHash(withReference(ref, fields...)...), requestHash(fields...)) {
		t.Fatal("hash com referência igual ao sem referência")
	}
}
