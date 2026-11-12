/**
 * `bigint` do protobuf → inteiro do JSON (regra 2: dinheiro é inteiro em centavos). Estourar o
 * limite seguro do JS é falha, nunca arredondamento.
 */
export function centsToNumber(cents: bigint): number {
  if (cents > BigInt(Number.MAX_SAFE_INTEGER) || cents < BigInt(Number.MIN_SAFE_INTEGER)) {
    throw new RangeError("valor em centavos fora do intervalo inteiro seguro do JSON");
  }
  return Number(cents);
}
