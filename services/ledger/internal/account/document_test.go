package account_test

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"conta.local/ledger/internal/account"
)

func TestParseCPF(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "válido", in: "11144477735"},
		{name: "válido com primeiro dígito verificador zero", in: "52998224725"},
		{name: "com pontuação", in: "111.444.777-35", wantErr: true},
		{name: "curto", in: "1114447773", wantErr: true},
		{name: "longo", in: "111444777350", wantErr: true},
		{name: "com letra", in: "1114447773a", wantErr: true},
		{name: "vazio", in: "", wantErr: true},
		{name: "todos iguais", in: "11111111111", wantErr: true},
		{name: "zeros", in: "00000000000", wantErr: true},
		{name: "primeiro verificador errado", in: "11144477745", wantErr: true},
		{name: "segundo verificador errado", in: "11144477736", wantErr: true},
		{name: "dígito não ASCII", in: "1114447773٥", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := account.ParseCPF(tt.in)
			if tt.wantErr {
				var verr *account.ValidationError
				if !errors.As(err, &verr) || verr.Field != "holder_document" {
					t.Fatalf("ParseCPF(%q) err = %v, want ValidationError em holder_document", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCPF(%q) err = %v", tt.in, err)
			}
			if doc.Digits() != tt.in {
				t.Errorf("Digits() = %q, want %q", doc.Digits(), tt.in)
			}
		})
	}
}

func TestDocumentNeverLeaksWhenFormatted(t *testing.T) {
	doc, err := account.ParseCPF("11144477735")
	if err != nil {
		t.Fatal(err)
	}
	const want = "***.444.777-**"
	if doc.Masked() != want {
		t.Fatalf("Masked() = %q, want %q", doc.Masked(), want)
	}

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).InfoContext(t.Context(), "x", "holder", doc)

	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", doc),
		"%s":   fmt.Sprintf("%s", doc), //nolint:staticcheck // o verbo %s é justamente o que está sendo testado.
		"%+v":  fmt.Sprintf("%+v", doc),
		"%#v":  fmt.Sprintf("%#v", doc),
		"slog": logged.String(),
	}
	for verb, out := range outputs {
		if strings.Contains(out, "11144477735") || strings.Contains(out, "111444") {
			t.Errorf("%s vazou o CPF: %q", verb, out)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s = %q, want conter %q", verb, out, want)
		}
	}
}
