# Protos de terceiros: googleapis

`rpc/error_details.proto` e `rpc/status.proto` vêm de https://github.com/googleapis/googleapis
(Apache 2.0, cabeçalho preservado), commit `ebd1d23ac613b177828dad42ad8dfb13ba498279`.

- Servem para o BFF decodificar o `google.rpc.ErrorInfo` dos erros do ledger (ADR-0006).
- Geram **só TypeScript** (`buf.gen.yaml`): o Go usa o `errdetails` do `genproto`.
- Ficam fora do `buf lint`, do `buf format` e do `buf breaking`. Não edite à mão.
- Para atualizar: baixe os dois arquivos no novo commit, troque o commit acima e rode `make generate`.
