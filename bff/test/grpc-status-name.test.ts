import { Code } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { grpcStatusName } from "../src/platform/connect-telemetry.js";

// O rótulo de status precisa ser o mesmo que o otelgrpc do ledger emite (semconv v1.43):
// nome canônico do gRPC, em maiúsculas, com a grafia "CANCELLED".
describe("grpcStatusName", () => {
  it.each([
    [0, "OK"],
    [Code.Canceled, "CANCELLED"],
    [Code.InvalidArgument, "INVALID_ARGUMENT"],
    [Code.DeadlineExceeded, "DEADLINE_EXCEEDED"],
    [Code.FailedPrecondition, "FAILED_PRECONDITION"],
    [Code.Unavailable, "UNAVAILABLE"],
    [Code.Unauthenticated, "UNAUTHENTICATED"],
  ])("código %d vira %s", (code, name) => {
    expect(grpcStatusName(code)).toBe(name);
  });

  it("código fora da faixa vira UNKNOWN, sem criar rótulo novo", () => {
    expect(grpcStatusName(99)).toBe("UNKNOWN");
  });
});
