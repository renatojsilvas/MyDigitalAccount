#!/bin/sh
# Critério de pronto da Fase 0: um /ping do BFF ao ledger vira UM trace só, e os logs dos
# dois serviços carregam o mesmo trace_id (o salto do log ao trace).
#
# O script manda ao BFF um traceparent com um trace_id conhecido (o BFF continua o trace de
# quem o chama), então sabemos exatamente qual trace procurar. Pré-requisitos: profiles core
# e obs no ar, com OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 nos serviços:
#
#   OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 \
#     docker compose --profile core --profile obs up -d --build
#   docker compose run --rm tools make obs-e2e
set -eu

BFF="${BFF_URL:-http://bff:8088}"
TEMPO="${TEMPO_URL:-http://tempo:3200}"
LOKI="${LOKI_URL:-http://loki:3100}"

hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }
trace_id="$(hex 16)"
caller_span="$(hex 8)"

# Espera o BFF estar pronto (até 60 s): logo após `up`, ele ainda pode estar subindo.
i=0
until curl -fsS -o /dev/null "${BFF}/readyz" 2>/dev/null; do
  i=$((i + 1))
  [ "${i}" -ge 60 ] && { echo "FALHOU: o BFF não respondeu em ${BFF}/readyz; o profile core está no ar? (docker compose --profile core ps)"; exit 1; }
  sleep 1
done

status="$(curl -s -o /dev/null -w '%{http_code}' -H "traceparent: 00-${trace_id}-${caller_span}-01" "${BFF}/ping?message=e2e" || true)"
[ "${status}" = "200" ] || { echo "FALHOU: GET /ping devolveu '${status}' (esperado 200); o ledger está saudável?"; exit 1; }
echo "GET /ping ok; trace_id=${trace_id}"

# A API v2 do Tempo devolve os ids de span em base64. Comparamos tudo em base64 (sem
# converter para hexadecimal: são bytes aleatórios e qualquer passagem por texto os corrompe).
# Colunas: serviço, tipo do span, id do span, id do pai.
spans_of_trace() {
  curl -fsS "${TEMPO}/api/v2/traces/${trace_id}" | jq -r '
    .trace.resourceSpans[] as $rs
    | $rs.scopeSpans[].spans[]
    | [($rs.resource.attributes[] | select(.key == "service.name") | .value.stringValue),
       .kind, .spanId, (.parentSpanId // "")]
    | @tsv'
}

# O span que injetamos como "quem chamou", em base64, para comparar com o pai do span do BFF.
# (/usr/bin/printf: o printf embutido do dash não entende \x.)
caller_span_b64="$(/usr/bin/printf "$(printf '%s' "${caller_span}" | sed 's/../\\x&/g')" | base64)"

# Espera o Collector e o Tempo terminarem de processar (até 30 s) por 3 spans.
i=0
while :; do
  spans="$(spans_of_trace 2>/dev/null || true)"
  [ "$(printf '%s\n' "${spans}" | grep -c .)" -ge 3 ] && break
  i=$((i + 1))
  [ "${i}" -ge 30 ] && {
    echo "FALHOU: o trace não chegou ao Tempo com 3 spans (spans vistos: $(printf '%s\n' "${spans}" | grep -c .))."
    echo "Os serviços estão exportando? Suba com OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317 (no .env ou na linha de comando)."
    exit 1
  }
  sleep 1
done

# Tipo do span: SERVER ou CLIENT (o JSON do Tempo traz o nome SPAN_KIND_*; aceitamos o número também).
bff_server="$(printf '%s\n' "${spans}" | awk -F'\t' '$1 == "bff" && ($2 == 2 || $2 == "SPAN_KIND_SERVER")')"
bff_client="$(printf '%s\n' "${spans}" | awk -F'\t' '$1 == "bff" && ($2 == 3 || $2 == "SPAN_KIND_CLIENT")')"
ledger_server="$(printf '%s\n' "${spans}" | awk -F'\t' '$1 == "ledger" && ($2 == 2 || $2 == "SPAN_KIND_SERVER")')"
[ -n "${bff_server}" ] && [ -n "${bff_client}" ] && [ -n "${ledger_server}" ] || {
  echo "FALHOU: faltam spans (esperado: bff servidor, bff cliente, ledger servidor)"; printf '%s\n' "${spans}"; exit 1; }

field() { printf '%s' "$1" | cut -f"$2"; }
[ "$(field "${bff_server}" 4)" = "${caller_span_b64}" ] || { echo "FALHOU: o span do BFF não continua o traceparent de quem chamou"; exit 1; }
[ "$(field "${bff_client}" 4)" = "$(field "${bff_server}" 3)" ] || { echo "FALHOU: o span de cliente não é filho do span do servidor do BFF"; exit 1; }
[ "$(field "${ledger_server}" 4)" = "$(field "${bff_client}" 3)" ] || { echo "FALHOU: o span do ledger não é filho do span de cliente do BFF (traceparent não chegou)"; exit 1; }
echo "ok: um único trace, com bff (servidor) -> bff (cliente gRPC) -> ledger (servidor), encadeados"

# Logs: os dois serviços escreveram uma linha com este trace_id.
log_services() {
  curl -fsSG "${LOKI}/loki/api/v1/query_range" --data-urlencode "query={service_name=~\".+\"} | trace_id=\"${trace_id}\"" \
    --data-urlencode "since=1h" --data-urlencode "limit=20" | jq -r '.data.result[].stream.service_name' | sort -u
}
i=0
while :; do
  services="$(log_services 2>/dev/null || true)"
  [ "${services}" = "$(printf 'bff\nledger')" ] && break
  i=$((i + 1))
  [ "${i}" -ge 30 ] && { echo "FALHOU: logs com o trace_id vindos de: '${services}' (esperado: bff e ledger)"; exit 1; }
  sleep 1
done
echo "ok: logs do bff e do ledger com o mesmo trace_id no Loki"
echo "critério de pronto da Fase 0 OK: o ping aparece como um único trace, ligado aos logs"
