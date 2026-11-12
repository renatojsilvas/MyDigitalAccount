#!/bin/sh
# Critério de pronto da Fase 3, na stack real (BFF, transfers, antifraude, ledger, spi-fake):
#
#   1. um Pix pelo BFF é UM trace com os cinco serviços;
#   2. repetir o pedido com a mesma Idempotency-Key devolve o mesmo Pix e debita uma vez só;
#   3. recusa explícita na liquidação termina em estorno (saldo de volta);
#   4. timeout na liquidação nunca estorna nem conclui sem confirmação (fica pendente, com o débito).
#
# Pré-requisitos: profiles core e obs no ar (com OTEL_EXPORTER_OTLP_ENDPOINT) e o seed feito:
#
#   OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 docker compose --profile core --profile obs up -d --build
#   docker compose run --rm tools make seed
#   docker compose run --rm tools make pix-e2e
#
# Muda o modo do spi-fake durante o teste e o devolve a "settle" no fim, mesmo se falhar.
set -eu

BFF="${BFF_URL:-http://bff:8088}"
TEMPO="${TEMPO_URL:-http://tempo:3200}"
SPI="${SPI_ADDR:-spi-fake:50071}"
ANA_CPF=12345678909
BRUNO_CPF=98765432100

fail() { echo "FALHOU: $*"; exit 1; }
uuid() { cat /proc/sys/kernel/random/uuid; }
spi_mode() {
  grpcurl -plaintext -import-path proto -proto conta/spi/v1/spi.proto \
    -d "{\"behavior\":{\"settle_mode\":\"$1\"}}" "${SPI}" conta.spi.v1.SpiAdminService/SetBehavior >/dev/null
}
trap 'spi_mode SETTLE_MODE_SETTLE 2>/dev/null || true' EXIT

i=0
until curl -fsS -o /dev/null "${BFF}/readyz" 2>/dev/null; do
  i=$((i + 1)); [ "${i}" -ge 60 ] && fail "o BFF não respondeu em ${BFF}/readyz"; sleep 1
done
spi_mode SETTLE_MODE_SETTLE

TOKEN="$(curl -fsS -X POST "${BFF}/auth/demo-login" -H 'content-type: application/json' \
  -d "{\"document\":\"${ANA_CPF}\"}" | jq -r .accessToken)" || fail "login da Ana (o seed foi feito? make seed)"
auth="authorization: Bearer ${TOKEN}"
balance() { curl -fsS "${BFF}/home" -H "${auth}" | jq -r .balanceCents; }

# send <chave> <centavos>: imprime "<http> <pixId> <x-trace-id>"
send() {
  out="$(curl -s -D /tmp/pix-headers -X POST "${BFF}/pix" -H "${auth}" -H "idempotency-key: $1" \
    -H 'content-type: application/json' \
    -d "{\"destinationKey\":{\"type\":\"cpf\",\"value\":\"${BRUNO_CPF}\"},\"amountCents\":$2,\"description\":\"e2e fase 3\"}")"
  code="$(head -n1 /tmp/pix-headers | awk '{print $2}')"
  tid="$(tr -d '\r' < /tmp/pix-headers | awk 'tolower($1) == "x-trace-id:" {print $2}')"
  echo "${code} $(printf '%s' "${out}" | jq -r .pixId) ${tid}"
}
# wait_for <pixId> <segundos> <condição jq>: espera o Pix satisfazer a condição; imprime o Pix.
wait_for() {
  i=0
  while :; do
    pix="$(curl -fsS "${BFF}/pix/$1" -H "${auth}")"
    printf '%s' "${pix}" | jq -e "$3" >/dev/null && { printf '%s' "${pix}"; return 0; }
    i=$((i + 1)); [ "${i}" -ge "$(( $2 * 4 ))" ] && { printf '%s' "${pix}"; return 1; }
    sleep 0.25
  done
}

# 1 e 2: Pix normal, um trace, e a repetição com a mesma chave.
before="$(balance)"
key="$(uuid)"
set -- $(send "${key}" 1234)
[ "$1" = 202 ] || fail "POST /pix devolveu $1 (esperado 202)"
pix_id="$2"; trace_id="$3"
done_pix="$(wait_for "${pix_id}" 30 '.status != "processing"')" || fail "o Pix não terminou em 30 s: ${done_pix}"
[ "$(printf '%s' "${done_pix}" | jq -r .status)" = completed ] || fail "Pix ${pix_id} terminou como $(printf '%s' "${done_pix}" | jq -c '{status,reason}')"
set -- $(send "${key}" 1234)
[ "$1" = 202 ] && [ "$2" = "${pix_id}" ] || fail "a repetição com a mesma chave devolveu $1 $2 (esperado 202 ${pix_id})"
after="$(balance)"
[ $((before - after)) -eq 1234 ] || fail "saldo caiu $((before - after)) (esperado 1234: um débito só)"
echo "ok: Pix ${pix_id} concluído; a mesma Idempotency-Key devolveu o mesmo Pix e o saldo caiu uma vez só"

expected="$(printf 'antifraude\nbff\nledger\nspi-fake\ntransfers')"
i=0
while :; do
  services="$(curl -fsS "${TEMPO}/api/v2/traces/${trace_id}" 2>/dev/null | jq -r \
    '[.trace.resourceSpans[]?.resource.attributes[] | select(.key == "service.name") | .value.stringValue] | unique | .[]' \
    2>/dev/null || true)"
  [ "${services}" = "${expected}" ] && break
  i=$((i + 1)); [ "${i}" -ge 30 ] && fail "trace ${trace_id} com os serviços '$(echo ${services})' (esperado: $(echo ${expected}))"
  sleep 1
done
echo "ok: um único trace (${trace_id}) com bff, transfers, antifraude, ledger e spi-fake"

# 3: recusa explícita do SPI termina em estorno, e o saldo volta.
spi_mode SETTLE_MODE_REJECT
before="$(balance)"
set -- $(send "$(uuid)" 777)
rejected="$(wait_for "$2" 30 '.status != "processing"')" || fail "o Pix recusado não terminou em 30 s: ${rejected}"
[ "$(printf '%s' "${rejected}" | jq -r '.status + " " + .state + " " + .reason')" = "failed reversed settlement_rejected" ] \
  || fail "recusa do SPI terminou como $(printf '%s' "${rejected}" | jq -c '{status,state,reason}')"
[ "$(balance)" -eq "${before}" ] || fail "depois do estorno o saldo é $(balance) (esperado ${before})"
echo "ok: recusa explícita na liquidação terminou em estorno, com o saldo de volta"

# 4: o SPI não liquida nem responde: o Pix fica pendente, com o débito, sem estorno.
spi_mode SETTLE_MODE_NO_SETTLE_NO_RESPONSE
before="$(balance)"
set -- $(send "$(uuid)" 555)
pending="$(wait_for "$2" 30 '.state == "pending_confirmation"')" || fail "o Pix não ficou pendente: $(printf '%s' "${pending}" | jq -c '{status,state}')"
sleep 5 # algumas consultas de status depois: continua pendente
pending="$(curl -fsS "${BFF}/pix/$2" -H "${auth}")"
[ "$(printf '%s' "${pending}" | jq -r '.status + " " + .state')" = "processing pending_confirmation" ] \
  || fail "timeout na liquidação mudou o Pix para $(printf '%s' "${pending}" | jq -c '{status,state,reason}')"
[ $((before - $(balance))) -eq 555 ] || fail "o débito do Pix pendente não foi mantido"
echo "ok: timeout na liquidação deixou o Pix pendente, com o débito, sem estornar nem concluir"
echo "critério de pronto da Fase 3 OK"
