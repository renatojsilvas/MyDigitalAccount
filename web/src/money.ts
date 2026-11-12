/**
 * Centavos (inteiro) → "R$ 1.234,56". Só apresentação: o valor vem pronto do BFF e a regra 2 do
 * projeto vale aqui também, então não há divisão em ponto flutuante: a separação em reais e
 * centavos é feita com BigInt.
 */
export function formatBRL(cents: number): string {
  const value = BigInt(cents); // lança se não for inteiro: o contrato garante que é
  const negative = value < 0n;
  const abs = negative ? -value : value;
  const reais = new Intl.NumberFormat("pt-BR").format(abs / 100n);
  const centavos = (abs % 100n).toString().padStart(2, "0");
  return `${negative ? "-" : ""}R$ ${reais},${centavos}`;
}
