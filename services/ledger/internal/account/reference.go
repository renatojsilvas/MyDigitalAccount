package account

const maxExternalReferenceLen = 64

// ExternalReference é o ID do negócio que originou um lançamento (o transfer_id, no Pix),
// usado pela conciliação para casar os sistemas (ADR-0009, seção 3). Opcional: o valor zero é
// "sem referência". Não é único: um Pix gera mais de um lançamento com a mesma referência.
type ExternalReference struct {
	value string
}

// NewExternalReference valida a referência: vazia (sem referência) ou de 1 a 64 caracteres
// ASCII visíveis, a mesma regra do CHECK no banco.
func NewExternalReference(s string) (ExternalReference, error) {
	if len(s) > maxExternalReferenceLen {
		return ExternalReference{}, invalid("external_reference", "is too long")
	}
	for i := range len(s) {
		if s[i] < '!' || s[i] > '~' {
			return ExternalReference{}, invalid("external_reference", "must have only visible ASCII characters")
		}
	}
	return ExternalReference{value: s}, nil
}

// IsZero diz se não há referência.
func (r ExternalReference) IsZero() bool { return r.value == "" }

func (r ExternalReference) String() string { return r.value }
