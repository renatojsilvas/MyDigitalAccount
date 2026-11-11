package account

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxHolderNameRunes  = 120
	maxDescriptionRunes = 140
)

// HolderName é o nome do titular, sem espaços nas pontas e sem caracteres de controle.
type HolderName struct {
	value string
}

// NewHolderName valida o nome do titular: obrigatório, até 120 caracteres.
func NewHolderName(s string) (HolderName, error) {
	v, err := cleanText("holder_name", s, maxHolderNameRunes)
	if err != nil {
		return HolderName{}, err
	}
	if v == "" {
		return HolderName{}, invalid("holder_name", "is required")
	}
	return HolderName{value: v}, nil
}

func (n HolderName) String() string {
	return n.value
}

// Description é o texto curto que aparece no extrato. Pode ser vazio.
type Description struct {
	value string
}

// NewDescription valida a descrição: opcional, até 140 caracteres.
func NewDescription(s string) (Description, error) {
	v, err := cleanText("description", s, maxDescriptionRunes)
	if err != nil {
		return Description{}, err
	}
	return Description{value: v}, nil
}

func (d Description) String() string {
	return d.value
}

// cleanText recusa UTF-8 inválido e caracteres de controle (que quebrariam extrato e log)
// e corta os espaços das pontas antes de medir o tamanho.
func cleanText(field, s string, maxRunes int) (string, error) {
	if !utf8.ValidString(s) {
		return "", invalid(field, "must be valid UTF-8")
	}
	v := strings.TrimSpace(s)
	if strings.IndexFunc(v, unicode.IsControl) >= 0 {
		return "", invalid(field, "must not contain control characters")
	}
	if utf8.RuneCountInString(v) > maxRunes {
		return "", invalid(field, "is too long")
	}
	return v, nil
}
