package grpcapi

import (
	"errors"
	"fmt"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	ledgerv1 "conta.local/gen/go/conta/ledger/v1"
	"conta.local/ledger/internal/account"
)

// Codec implementa account.ResultCodec com o próprio contrato: o resultado gravado na
// tabela de idempotência é a mensagem da resposta em Protobuf (OK) ou o google.rpc.Status
// com o ErrorInfo (recusa), produzido pela mesma statusFor que responde ao cliente.
type Codec struct{}

var _ account.ResultCodec = Codec{}

// EncodeAccount grava a conta como ledgerv1.Account.
func (Codec) EncodeAccount(a account.Account) ([]byte, error) {
	return proto.Marshal(accountToProto(a))
}

// DecodeAccount reconstrói a conta gravada, passando de novo pelos construtores do domínio.
func (Codec) DecodeAccount(b []byte) (account.Account, error) {
	var m ledgerv1.Account
	if err := proto.Unmarshal(b, &m); err != nil {
		return account.Account{}, fmt.Errorf("grpcapi: conta gravada ilegível: %w", err)
	}
	id, err := account.ParseID(m.GetAccountId())
	if err != nil {
		return account.Account{}, err
	}
	holder, err := account.NewHolderName(m.GetHolderName())
	if err != nil {
		return account.Account{}, err
	}
	doc, err := account.ParseCPF(m.GetHolderDocument())
	if err != nil {
		return account.Account{}, err
	}
	if m.GetStatus() != ledgerv1.AccountStatus_ACCOUNT_STATUS_ACTIVE {
		return account.Account{}, fmt.Errorf("grpcapi: status gravado inesperado %v", m.GetStatus())
	}
	return account.Open(id, holder, doc, m.GetCreatedAt().AsTime()), nil
}

// EncodePosting grava a perna como ledgerv1.Posting, a mensagem que vai na resposta.
func (Codec) EncodePosting(p account.Posting) ([]byte, error) {
	return proto.Marshal(postingToProto(p))
}

// DecodePosting reconstrói a perna gravada.
func (Codec) DecodePosting(b []byte) (account.Posting, error) {
	var m ledgerv1.Posting
	if err := proto.Unmarshal(b, &m); err != nil {
		return account.Posting{}, fmt.Errorf("grpcapi: perna gravada ilegível: %w", err)
	}
	entry, err := account.ParseID(m.GetEntryId())
	if err != nil {
		return account.Posting{}, err
	}
	id, err := account.ParseID(m.GetAccountId())
	if err != nil {
		return account.Posting{}, err
	}
	dir, err := directionFromProto(m.GetDirection())
	if err != nil {
		return account.Posting{}, err
	}
	desc, err := account.NewDescription(m.GetDescription())
	if err != nil {
		return account.Posting{}, err
	}
	reason, err := entryReasonFromProto(m.GetReason())
	if err != nil {
		return account.Posting{}, err
	}
	ref, err := account.NewExternalReference(m.GetExternalReference())
	if err != nil {
		return account.Posting{}, err
	}
	var reverses account.EntryID
	if s := m.GetReversesEntryId(); s != "" {
		if reverses, err = account.ParseEntryID(s); err != nil {
			return account.Posting{}, err
		}
	}
	return account.Posting{
		EntryID: account.EntryID(entry), AccountID: id, Direction: dir,
		Amount: account.MoneyFromCents(m.GetAmountCents()), BalanceAfter: account.MoneyFromCents(m.GetBalanceAfterCents()),
		Description: desc, PostedAt: m.GetPostedAt().AsTime(),
		Reason: reason, Reference: ref, ReversesEntryID: reverses,
	}, nil
}

// EncodeRejection grava a recusa como o google.rpc.Status que o cliente recebeu.
func (Codec) EncodeRejection(err error) (int32, []byte, error) {
	st, expected := statusFor(err)
	if !expected || st.Code() == codes.OK {
		return 0, nil, fmt.Errorf("grpcapi: %w não é uma recusa de negócio", err)
	}
	body, mErr := proto.Marshal(st.Proto())
	if mErr != nil {
		return 0, nil, mErr
	}
	return int32(st.Code()), body, nil //nolint:gosec // códigos gRPC vão de 0 a 16
}

// DecodeRejection devolve a recusa gravada como erro de status gRPC, que a borda repassa
// sem traduzir de novo.
func (Codec) DecodeRejection(statusCode int32, body []byte) error {
	var m spb.Status
	if err := proto.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("grpcapi: recusa gravada ilegível: %w", err)
	}
	if m.GetCode() != statusCode {
		return errors.New("grpcapi: recusa gravada com código divergente")
	}
	return status.FromProto(&m).Err()
}
