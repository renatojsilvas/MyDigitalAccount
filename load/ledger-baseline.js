// Linha de base do ledger (Fase 1): carga moderada e constante, com thresholds que servem
// de gate no CI. Não é teste de estresse (isso é a Fase 7): é o "antes" contra o qual as
// próximas fases são comparadas, e o alarme de regressão de desempenho.
//
// Rodar (com o ledger no ar):
//   docker compose --profile core up -d --wait ledger
//   docker compose --profile load run --rm k6
import grpc from "k6/net/grpc";
import { check } from "k6";
import { Counter, Rate } from "k6/metrics";

const TARGET = __ENV.LEDGER_TARGET || "ledger:50051";
const SERVICE = "conta.ledger.v1.LedgerService";
const ACCOUNTS = 20;
const RATE = Number(__ENV.RATE || 40);
const DURATION_S = Number((__ENV.DURATION || "30s").replace(/s$/, ""));
// Leituras são 60% e escritas 40% das iterações. Mínimo de 90% do esperado em cada tipo.
const MIN_READS = Math.floor(RATE * DURATION_S * 0.6 * 0.9);
const MIN_WRITES = Math.floor(RATE * DURATION_S * 0.4 * 0.9);
// Saldo de sobra: nenhum débito da carga é recusado, então qualquer status diferente de OK é
// erro de verdade, não regra de negócio.
const OPENING_CENTS = 1_000_000_000;

export const options = {
  scenarios: {
    baseline: {
      // Chegada constante (modelo aberto): se o ledger ficar lento, o k6 não reduz o ritmo,
      // e a lentidão aparece na latência em vez de ser escondida (coordinated omission).
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: `${DURATION_S}s`,
      // VUs de sobra: numa pausa do ambiente, chamadas presas ocupam VUs, e criar VU no meio do
      // teste é caro (cada um carrega o .proto). Sem folga, o k6 descarta iterações.
      preAllocatedVUs: 50,
      maxVUs: 100,
    },
  },
  thresholds: {
    // p99 (pede o plano) com folga de ~2x sobre a pior rodada medida; p95, estável a pausas
    // isoladas do ambiente, pega degradações graduais. Origem dos números: load/README.md.
    "grpc_req_duration{op:read}": [`p(95)<${__ENV.P95_READ_MS || 25}`, `p(99)<${__ENV.P99_READ_MS || 100}`],
    "grpc_req_duration{op:write}": [`p(95)<${__ENV.P95_WRITE_MS || 50}`, `p(99)<${__ENV.P99_WRITE_MS || 150}`],
    ledger_unexpected_errors: ["rate<0.01"],
    checks: ["rate>0.99"],
    // Se o k6 não conseguir manter a taxa (VUs insuficientes), o teste não mediu o que diz.
    dropped_iterations: ["count==0"],
    // Um threshold sem amostras passa (o k6 não reprova o que não mediu): estes garantem que a
    // carga aconteceu de fato, com as tags esperadas, e o gate não fica verde por engano.
    "ledger_requests{op:read}": [`count>=${MIN_READS}`],
    "ledger_requests{op:write}": [`count>=${MIN_WRITES}`],
  },
  setupTimeout: "60s",
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],
};

const unexpectedErrors = new Rate("ledger_unexpected_errors");
const requests = new Counter("ledger_requests");
let seq = 0;

const client = new grpc.Client();
client.load(["/proto"], "conta/ledger/v1/ledger.proto");

// CPF válido aleatório: 9 dígitos sorteados e os dois verificadores calculados.
function randomCPF() {
  const d = Array.from({ length: 9 }, () => Math.floor(Math.random() * 10));
  for (const n of [9, 10]) {
    const sum = d.slice(0, n).reduce((acc, v, i) => acc + v * (n + 1 - i), 0);
    d.push(((sum * 10) % 11) % 10);
  }
  return d.join("");
}

// Chave única por chamada. __VU e __ITER não existem no setup(), então o contador é próprio.
function key(prefix) {
  seq += 1;
  return `k6-${prefix}-${__VU}-${seq}-${Date.now()}-${Math.floor(Math.random() * 1e9)}`;
}

function call(method, req, op) {
  const res = client.invoke(`${SERVICE}/${method}`, req, { tags: { op }, timeout: "5s" });
  const ok = check(res, { [`${method} OK`]: (r) => r && r.status === grpc.StatusOK });
  unexpectedErrors.add(!ok, { op });
  requests.add(1, { op });
  return res;
}

// Contas próprias do teste, criadas pela API (não usa as do seed).
export function setup() {
  client.connect(TARGET, { plaintext: true, timeout: "10s" });
  const ids = [];
  while (ids.length < ACCOUNTS) {
    const res = client.invoke(`${SERVICE}/OpenAccount`, {
      idempotency_key: key("open"),
      holder_name: `Carga ${ids.length + 1}`,
      holder_document: randomCPF(),
    });
    if (res.status === grpc.StatusAlreadyExists) continue; // CPF sorteado já existe: sorteia outro
    if (res.status !== grpc.StatusOK) throw new Error(`OpenAccount: ${JSON.stringify(res.error)}`);
    const id = res.message.account.accountId;
    const credit = client.invoke(`${SERVICE}/Credit`, {
      idempotency_key: key("opening"),
      account_id: id,
      amount_cents: OPENING_CENTS,
      reason: "CREDIT_REASON_OPENING",
    });
    if (credit.status !== grpc.StatusOK) throw new Error(`Credit: ${JSON.stringify(credit.error)}`);
    ids.push(id);
  }
  client.close();
  return { ids };
}

export default function (data) {
  // Uma conexão por VU, reaproveitada entre iterações (connect não pode rodar no init).
  if (__ITER === 0) client.connect(TARGET, { plaintext: true, timeout: "10s" });

  const id = data.ids[Math.floor(Math.random() * data.ids.length)];
  const r = Math.random();
  if (r < 0.4) {
    call("GetBalance", { account_id: id }, "read");
  } else if (r < 0.6) {
    call("ListStatement", { account_id: id, page_size: 20 }, "read");
  } else if (r < 0.8) {
    call("Credit", { idempotency_key: key("c"), account_id: id, amount_cents: 100, reason: "CREDIT_REASON_EXTERNAL" }, "write");
  } else {
    call("Debit", { idempotency_key: key("d"), account_id: id, amount_cents: 50, reason: "DEBIT_REASON_EXTERNAL" }, "write");
  }
}
