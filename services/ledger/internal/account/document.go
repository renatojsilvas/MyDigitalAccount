package account

import "log/slog"

// Document é o CPF do titular, só dígitos e com dígitos verificadores válidos.
//
// Formatá-lo (fmt, slog) devolve sempre a versão mascarada: o CPF completo só sai por
// Digits, chamado de propósito por quem precisa dele (persistência e resposta da API).
type Document struct {
	digits string
}

// ParseCPF valida um CPF com 11 dígitos, sem pontuação.
func ParseCPF(s string) (Document, error) {
	if len(s) != 11 {
		return Document{}, invalid("holder_document", "must have exactly 11 digits")
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return Document{}, invalid("holder_document", "must have only digits")
		}
	}
	if allSame(s) {
		return Document{}, invalid("holder_document", "is not a valid CPF")
	}
	if cpfCheckDigit(s[:9]) != s[9] || cpfCheckDigit(s[:10]) != s[10] {
		return Document{}, invalid("holder_document", "is not a valid CPF")
	}
	return Document{digits: s}, nil
}

// Digits devolve o CPF completo. Nunca use em log ou trace.
func (d Document) Digits() string {
	return d.digits
}

// Masked devolve o CPF no formato ***.456.789-**, que mantém só os dígitos do meio.
func (d Document) Masked() string {
	if len(d.digits) != 11 {
		return "***.***.***-**"
	}
	return "***." + d.digits[3:6] + "." + d.digits[6:9] + "-**"
}

// String implementa fmt.Stringer com a versão mascarada.
func (d Document) String() string {
	return d.Masked()
}

// GoString cobre o verbo %#v, que de outro modo imprimiria o campo interno.
func (d Document) GoString() string {
	return d.Masked()
}

// LogValue implementa slog.LogValuer: o CPF sai mascarado em qualquer chave de log.
func (d Document) LogValue() slog.Value {
	return slog.StringValue(d.Masked())
}

// cpfCheckDigit calcula o próximo dígito verificador sobre os dígitos já conhecidos
// (9 para o primeiro, 10 para o segundo), com pesos decrescentes até 2.
func cpfCheckDigit(digits string) byte {
	sum := 0
	weight := len(digits) + 1
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

func allSame(s string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			return false
		}
	}
	return true
}
