package account

// Contas internas, com identificadores fixos criados pela migration. Uma por finalidade, para
// que cada contrapartida seja conciliável contra a sua própria fonte (ADR-0004, seção 1).
var (
	// OpeningBalanceAccountID é a contrapartida do saldo inicial (seed).
	OpeningBalanceAccountID = ID{15: 1}
	// SettlementAccountID é a contrapartida do dinheiro que entra e sai pelo SPI.
	SettlementAccountID = ID{15: 2}
)

// EntryReason diz o que um lançamento é (ADR-0009, seção 2). Só o ledger o produz: a partir
// do motivo do pedido (crédito e débito) ou da operação (estorno).
type EntryReason int

// Motivos de lançamento.
const (
	ReasonOpening EntryReason = iota + 1
	ReasonExternal
	ReasonReversal
)

// String devolve o nome estável do motivo, usado no banco, em métricas e em traces.
func (r EntryReason) String() string {
	switch r {
	case ReasonOpening:
		return "opening"
	case ReasonExternal:
		return "external"
	case ReasonReversal:
		return "reversal"
	default:
		return "unknown"
	}
}

// CreditReason é o motivo de um crédito; define a conta interna de contrapartida.
type CreditReason int

// Motivos de crédito.
const (
	CreditOpening CreditReason = iota + 1
	CreditExternal
)

// DebitReason é o motivo de um débito; define a conta interna de contrapartida.
type DebitReason int

// Motivos de débito.
const (
	DebitExternal DebitReason = iota + 1
)

// counterpart é a regra contábil motivo -> conta interna. Fica no domínio porque quem chama
// a API nunca escolhe a contrapartida.
func (r CreditReason) counterpart() (ID, error) {
	switch r {
	case CreditOpening:
		return OpeningBalanceAccountID, nil
	case CreditExternal:
		return SettlementAccountID, nil
	default:
		return ID{}, invalid("reason", "is not a known credit reason")
	}
}

func (r CreditReason) entryReason() EntryReason {
	if r == CreditOpening {
		return ReasonOpening
	}
	return ReasonExternal
}

func (r DebitReason) counterpart() (ID, error) {
	switch r {
	case DebitExternal:
		return SettlementAccountID, nil
	default:
		return ID{}, invalid("reason", "is not a known debit reason")
	}
}
