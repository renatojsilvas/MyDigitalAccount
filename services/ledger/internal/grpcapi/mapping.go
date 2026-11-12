package grpcapi

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
)

// Conversões entre o contrato (.proto) e o domínio. Entrada: todo campo passa por um
// construtor do domínio, que devolve ValidationError (INVALID_ARGUMENT). Saída: o domínio já é
// válido, então a conversão não falha.

func creditReasonFromProto(r ledgerv1.CreditReason) (account.CreditReason, error) {
	switch r {
	case ledgerv1.CreditReason_CREDIT_REASON_OPENING:
		return account.CreditOpening, nil
	case ledgerv1.CreditReason_CREDIT_REASON_EXTERNAL:
		return account.CreditExternal, nil
	default:
		return 0, &account.ValidationError{Field: "reason", Problem: "must be a known credit reason"}
	}
}

func debitReasonFromProto(r ledgerv1.DebitReason) (account.DebitReason, error) {
	if r == ledgerv1.DebitReason_DEBIT_REASON_EXTERNAL {
		return account.DebitExternal, nil
	}
	return 0, &account.ValidationError{Field: "reason", Problem: "must be a known debit reason"}
}

func entryReasonToProto(r account.EntryReason) ledgerv1.EntryReason {
	switch r {
	case account.ReasonOpening:
		return ledgerv1.EntryReason_ENTRY_REASON_OPENING
	case account.ReasonExternal:
		return ledgerv1.EntryReason_ENTRY_REASON_EXTERNAL
	case account.ReasonReversal:
		return ledgerv1.EntryReason_ENTRY_REASON_REVERSAL
	default:
		return ledgerv1.EntryReason_ENTRY_REASON_UNSPECIFIED
	}
}

// entryReasonFromProto aceita UNSPECIFIED: respostas gravadas na tabela de idempotência antes
// da migration 00002 não têm motivo (ADR-0009, consequências).
func entryReasonFromProto(r ledgerv1.EntryReason) (account.EntryReason, error) {
	switch r {
	case ledgerv1.EntryReason_ENTRY_REASON_UNSPECIFIED:
		return 0, nil
	case ledgerv1.EntryReason_ENTRY_REASON_OPENING:
		return account.ReasonOpening, nil
	case ledgerv1.EntryReason_ENTRY_REASON_EXTERNAL:
		return account.ReasonExternal, nil
	case ledgerv1.EntryReason_ENTRY_REASON_REVERSAL:
		return account.ReasonReversal, nil
	default:
		return 0, fmt.Errorf("grpcapi: motivo de lançamento desconhecido %v", r)
	}
}

func directionToProto(d account.Direction) ledgerv1.Direction {
	switch d {
	case account.DirectionDebit:
		return ledgerv1.Direction_DIRECTION_DEBIT
	case account.DirectionCredit:
		return ledgerv1.Direction_DIRECTION_CREDIT
	default:
		return ledgerv1.Direction_DIRECTION_UNSPECIFIED
	}
}

func directionFromProto(d ledgerv1.Direction) (account.Direction, error) {
	switch d {
	case ledgerv1.Direction_DIRECTION_DEBIT:
		return account.DirectionDebit, nil
	case ledgerv1.Direction_DIRECTION_CREDIT:
		return account.DirectionCredit, nil
	default:
		return 0, fmt.Errorf("grpcapi: sentido desconhecido %v", d)
	}
}

func statusToProto(s account.Status) ledgerv1.AccountStatus {
	if s == account.StatusActive {
		return ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	}
	return ledgerv1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED
}

func accountToProto(a account.Account) *ledgerv1.Account {
	s := a.Snapshot()
	return &ledgerv1.Account{
		AccountId:      s.ID.String(),
		HolderName:     s.Holder.String(),
		HolderDocument: s.Document.Digits(),
		Status:         statusToProto(s.Status),
		CreatedAt:      timestamppb.New(s.CreatedAt),
	}
}

func postingToProto(p account.Posting) *ledgerv1.Posting {
	var reverses string
	if !p.ReversesEntryID.IsZero() {
		reverses = p.ReversesEntryID.String()
	}
	return &ledgerv1.Posting{
		Reason:            entryReasonToProto(p.Reason),
		ExternalReference: p.Reference.String(),
		ReversesEntryId:   reverses,
		EntryId:           p.EntryID.String(),
		AccountId:         p.AccountID.String(),
		Direction:         directionToProto(p.Direction),
		AmountCents:       p.Amount.Cents(),
		BalanceAfterCents: p.BalanceAfter.Cents(),
		Description:       p.Description.String(),
		PostedAt:          timestamppb.New(p.PostedAt),
	}
}

// O page_token é opaco para o cliente: versão + posição (Seq) em base64. Trocar a forma de
// paginar no futuro só exige uma nova versão do token, não uma mudança no contrato.
const pageTokenV1 = 1

func encodePageToken(seq int64) string {
	if seq == 0 {
		return ""
	}
	b := make([]byte, 9)
	b[0] = pageTokenV1
	binary.BigEndian.PutUint64(b[1:], uint64(seq)) //nolint:gosec // Seq é positivo (identidade do banco)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePageToken(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 9 || b[0] != pageTokenV1 {
		return 0, &account.ValidationError{Field: "page_token", Problem: "is not valid"}
	}
	seq := int64(binary.BigEndian.Uint64(b[1:])) //nolint:gosec // conferido abaixo
	if seq <= 0 {
		return 0, &account.ValidationError{Field: "page_token", Problem: "is not valid"}
	}
	return seq, nil
}
