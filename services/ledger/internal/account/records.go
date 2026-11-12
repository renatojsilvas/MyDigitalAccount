package account

// StatementLine é uma linha do extrato: a perna da conta e a sua posição (Seq), usada como
// cursor da próxima página. Seq cresce na ordem em que o saldo mudou.
type StatementLine struct {
	Seq     int64
	Posting Posting
}

// StoredEntry é um lançamento gravado, lido para ser estornado.
type StoredEntry struct {
	ID     EntryID
	Reason EntryReason
	Legs   []Leg
	// CustomerAccountID é a conta de cliente do lançamento (zero se não houver).
	CustomerAccountID ID
}

// IdempotencyRecord é o registro de uma chave de idempotência (ADR-0004, seção 3).
//
// O resultado é opaco para o domínio: StatusCode e Body são o que a borda gRPC gravou
// (resposta em Protobuf quando OK, google.rpc.Status quando recusa). Completed é falso só
// dentro da transação que reservou a chave, antes de gravar o resultado.
type IdempotencyRecord struct {
	RequestHash []byte
	Completed   bool
	StatusCode  int32
	Body        []byte
}
