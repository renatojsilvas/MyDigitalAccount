package pix

import "log/slog"

// KeyType é o tipo da chave Pix. Na demonstração só existe CPF (ADR-0007, seção 8).
type KeyType int

// Tipos de chave.
const (
	KeyCPF KeyType = iota + 1
)

func (t KeyType) String() string {
	if t == KeyCPF {
		return "cpf"
	}
	return "unknown"
}

// Key é a chave Pix de destino. Formatá-la (fmt, slog) devolve sempre a versão mascarada: o
// valor completo só sai por Value, chamado de propósito (persistência e consulta ao ledger).
type Key struct {
	typ   KeyType
	value string
}

// NewKey valida a chave conforme o tipo.
func NewKey(t KeyType, value string) (Key, error) {
	if t != KeyCPF {
		return Key{}, invalid("destination_key.type", "must be a known key type")
	}
	if !validCPF(value) {
		return Key{}, invalid("destination_key.value", "is not a valid CPF (11 digits)")
	}
	return Key{typ: t, value: value}, nil
}

// Type devolve o tipo da chave.
func (k Key) Type() KeyType { return k.typ }

// Value devolve a chave completa. Nunca use em log, trace ou métrica.
func (k Key) Value() string { return k.value }

// Masked devolve a chave mascarada (CPF: ***.456.789-**).
func (k Key) Masked() string {
	if k.typ != KeyCPF || len(k.value) != 11 {
		return "***"
	}
	return "***." + k.value[3:6] + "." + k.value[6:9] + "-**"
}

// String implementa fmt.Stringer com a versão mascarada.
func (k Key) String() string { return k.Masked() }

// GoString cobre o verbo %#v, que de outro modo imprimiria o campo interno.
func (k Key) GoString() string { return k.Masked() }

// LogValue implementa slog.LogValuer: a chave sai mascarada em qualquer chave de log.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.Masked()) }

func validCPF(s string) bool {
	if len(s) != 11 {
		return false
	}
	same := true
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		same = same && s[i] == s[0]
	}
	return !same && cpfCheckDigit(s[:9]) == s[9] && cpfCheckDigit(s[:10]) == s[10]
}

// cpfCheckDigit calcula o próximo dígito verificador sobre os já conhecidos, com pesos
// decrescentes até 2.
func cpfCheckDigit(digits string) byte {
	sum, weight := 0, len(digits)+1
	for i := range len(digits) {
		sum += int(digits[i]-'0') * weight
		weight--
	}
	r := sum * 10 % 11
	if r == 10 {
		r = 0
	}
	return byte('0' + r)
}
