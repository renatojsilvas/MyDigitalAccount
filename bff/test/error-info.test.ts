import http2 from "node:http2";
import type { AddressInfo } from "node:net";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { connectNodeAdapter, createGrpcTransport } from "@connectrpc/connect-node";
import {
  ErrorReason,
  ErrorReasonSchema,
  LedgerService,
} from "@conta/contracts/conta/ledger/v1/ledger_pb";
import { ErrorInfoSchema } from "@conta/contracts/google/rpc/error_details_pb";
import { describe, expect, it } from "vitest";

// Nome do motivo como vai no fio (ex.: "ERROR_REASON_ACCOUNT_NOT_FOUND"), tirado do descritor
// gerado: o protoc-gen-es remove o prefixo dos nomes dos membros do enum em TS.
function reasonName(reason: number): string | undefined {
  return ErrorReasonSchema.values.find((v) => v.number === reason)?.name;
}

// Prova, em gRPC de verdade, o que o adaptador do ledger vai fazer: ler o google.rpc.ErrorInfo
// dos detalhes do erro com o tipo GERADO do .proto, e comparar o motivo com o enum gerado.
describe("google.rpc.ErrorInfo gerado do .proto", () => {
  it("chega pelo fio e o motivo bate com o enum ErrorReason", async () => {
    const handler = connectNodeAdapter({
      routes: (router) => {
        router.service(LedgerService, {
          getBalance: () => {
            throw new ConnectError("conta não encontrada", Code.NotFound, undefined, [
              {
                desc: ErrorInfoSchema,
                value: {
                  reason: reasonName(ErrorReason.ACCOUNT_NOT_FOUND),
                  domain: "ledger.conta.local",
                  metadata: { field: "account_id" },
                },
              },
            ]);
          },
        });
      },
    });
    const server = http2.createServer(handler);
    // O cliente mantém a sessão HTTP/2 aberta; é preciso destruí-la para o servidor fechar.
    const sessions = new Set<http2.ServerHttp2Session>();
    server.on("session", (session) => {
      sessions.add(session);
      session.on("close", () => sessions.delete(session));
    });
    await new Promise<void>((resolve) => {
      server.listen(0, "127.0.0.1", resolve);
    });
    const { port } = server.address() as AddressInfo;
    const transport = createGrpcTransport({ baseUrl: `http://127.0.0.1:${String(port)}` });

    try {
      await createClient(LedgerService, transport).getBalance({ accountId: "x" });
      expect.unreachable("deveria ter falhado");
    } catch (err) {
      const e = ConnectError.from(err);
      expect(e.code).toBe(Code.NotFound);
      const [info] = e.findDetails(ErrorInfoSchema);
      expect(info?.domain).toBe("ledger.conta.local");
      expect(info?.reason).toBe(reasonName(ErrorReason.ACCOUNT_NOT_FOUND));
      expect(info?.metadata.field).toBe("account_id");
    } finally {
      server.close();
      for (const session of sessions) session.destroy();
    }
  });
});
