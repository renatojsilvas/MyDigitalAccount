import { describe, expect, it } from "vitest";
import { formatBRL } from "./money";

describe("formatBRL", () => {
  it.each([
    [0, "R$ 0,00"],
    [5, "R$ 0,05"],
    [100, "R$ 1,00"],
    [123_456, "R$ 1.234,56"],
    [500_000, "R$ 5.000,00"],
    [-250, "-R$ 2,50"],
    // acima de 2^53 / 100, onde a divisão em ponto flutuante já erraria os centavos
    [9_007_199_254_740_991, "R$ 90.071.992.547.409,91"],
  ])("%d centavos → %s", (cents, expected) => {
    expect(formatBRL(cents)).toBe(expected);
  });
});
