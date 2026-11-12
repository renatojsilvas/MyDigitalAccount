package account

import "context"

// Portas do caso de uso, definidas aqui, por quem as consome. O adaptador internal/postgres
// as satisfaz sem importar nada além deste pacote.

// Store é o acesso ao banco do ledger.
type Store interface {
	// WithTx executa fn numa transação: commit se fn devolver nil, rollback caso contrário.
	WithTx(ctx context.Context, fn func(Tx) error) error
	// Account lê uma conta de cliente sem travar. ErrAccountNotFound se não existir.
	Account(ctx context.Context, id ID) (Account, error)
	// Statement lê até limit linhas com Seq menor que before (0 = desde a mais recente).
	Statement(ctx context.Context, id ID, before int64, limit int32) ([]StatementLine, error)
}

// Tx são as operações feitas dentro de uma transação de escrita.
type Tx interface {
	// ClaimKey reserva a chave; true se é nova. Espera por outra transação com a mesma chave.
	ClaimKey(ctx context.Context, scope, key string, requestHash []byte) (bool, error)
	KeyRecord(ctx context.Context, scope, key string) (IdempotencyRecord, error)
	CompleteKey(ctx context.Context, scope, key string, statusCode int32, body []byte) error

	// CreateAccount grava a conta. ErrAccountAlreadyExists se o CPF já tem conta.
	CreateAccount(ctx context.Context, a Account) error
	// LockAccount lê e trava a conta até o fim da transação. ErrAccountNotFound se não existir.
	LockAccount(ctx context.Context, id ID) (Account, error)
	SaveBalance(ctx context.Context, a Account) error
	AppendEntry(ctx context.Context, e Entry) error
}

// ResultCodec converte o resultado de uma escrita para o formato gravado na tabela de
// idempotência, e de volta. Quem o implementa é a borda gRPC: o resultado gravado é a
// própria resposta (status OK) ou o status com o ErrorInfo (recusa), para que a repetição
// seja idêntica à original. Para este pacote, são só bytes.
type ResultCodec interface {
	EncodeAccount(Account) ([]byte, error)
	DecodeAccount([]byte) (Account, error)
	EncodePosting(Posting) ([]byte, error)
	DecodePosting([]byte) (Posting, error)
	// EncodeRejection traduz uma recusa de negócio para o código de status e os bytes gravados.
	EncodeRejection(error) (statusCode int32, body []byte, err error)
	// DecodeRejection reconstrói, a partir do que foi gravado, o erro devolvido na repetição.
	DecodeRejection(statusCode int32, body []byte) error
}
