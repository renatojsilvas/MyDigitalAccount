package account

import (
	"fmt"

	"github.com/google/uuid"
)

// ID identifica uma conta (de cliente ou interna). Tipos distintos para conta e
// lançamento impedem trocar um pelo outro sem o compilador perceber.
type ID uuid.UUID

// EntryID identifica um lançamento (journal entry).
type EntryID uuid.UUID

// NewID gera um UUIDv7: ordenado no tempo, o que mantém o índice da chave primária
// compacto (inserções sempre no fim da árvore, em vez de espalhadas como no v4).
func NewID() (ID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return ID{}, fmt.Errorf("account: generate account id: %w", err)
	}
	return ID(id), nil
}

// NewEntryID gera um UUIDv7 para um lançamento.
func NewEntryID() (EntryID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return EntryID{}, fmt.Errorf("account: generate entry id: %w", err)
	}
	return EntryID(id), nil
}

// ParseID valida o identificador recebido de fora.
func ParseID(s string) (ID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return ID{}, invalid("account_id", "must be a UUID")
	}
	return ID(id), nil
}

// ParseEntryID valida o identificador de lançamento recebido de fora.
func ParseEntryID(s string) (EntryID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return EntryID{}, invalid("entry_id", "must be a UUID")
	}
	return EntryID(id), nil
}

// IsZero diz se o identificador não foi preenchido.
func (id EntryID) IsZero() bool { return id == EntryID{} }

func (id ID) String() string {
	return uuid.UUID(id).String()
}

func (id EntryID) String() string {
	return uuid.UUID(id).String()
}
