#!/bin/sh
# Smoke test da pipeline de observabilidade (ADR-0002).
#
# Envia um trace, uma métrica e um log sintéticos por OTLP ao Collector e confere que cada
# sinal chegou ao seu backend (Tempo, Prometheus, Loki) e que o Grafana enxerga os três
# datasources. Rode com o profile obs no ar:
#
#   docker compose --profile obs up -d --build
#   docker compose run --rm tools make obs-smoke
#
# O nome do serviço é único por execução, então rodar de novo nunca confunde dados antigos
# com novos.
set -eu

COLLECTOR="${COLLECTOR_URL:-http://otel-collector:4318}"
TEMPO="${TEMPO_URL:-http://tempo:3200}"
LOKI="${LOKI_URL:-http://loki:3100}"
PROM="${PROMETHEUS_URL:-http://prometheus:9090}"
GRAFANA="${GRAFANA_URL:-http://grafana:3000}"

hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

trace_id="$(hex 16)"
span_id="$(hex 8)"
service="smoke-$(hex 3)"
now="$(date +%s)000000000"
res="\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"${service}\"}}]}"

post() {
  curl -fsS -X POST -H 'Content-Type: application/json' "${COLLECTOR}/v1/$1" -d "$2" >/dev/null
}

post traces "{\"resourceSpans\":[{${res},\"scopeSpans\":[{\"spans\":[{\"traceId\":\"${trace_id}\",\"spanId\":\"${span_id}\",\"name\":\"smoke-span\",\"kind\":1,\"startTimeUnixNano\":\"${now}\",\"endTimeUnixNano\":\"${now}\"}]}]}]}"
post metrics "{\"resourceMetrics\":[{${res},\"scopeMetrics\":[{\"metrics\":[{\"name\":\"smoke.gauge\",\"gauge\":{\"dataPoints\":[{\"asDouble\":42,\"timeUnixNano\":\"${now}\"}]}}]}]}]}"
post logs "{\"resourceLogs\":[{${res},\"scopeLogs\":[{\"logRecords\":[{\"timeUnixNano\":\"${now}\",\"severityText\":\"INFO\",\"body\":{\"stringValue\":\"smoke log\"},\"traceId\":\"${trace_id}\",\"spanId\":\"${span_id}\"}]}]}]}"
echo "enviado: servico=${service} trace_id=${trace_id}"

# Espera até 30 s pelo sinal: o Collector agrupa em lote (2 s) e cada backend indexa depois.
expect() {
  name="$1"
  shift
  i=0
  until "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 30 ]; then
      echo "FALHOU: ${name}"
      exit 1
    fi
    sleep 1
  done
  echo "ok: ${name}"
}

tempo_has_trace() { curl -fsS "${TEMPO}/api/traces/${trace_id}"; }
loki_has_log() {
  curl -fsSG "${LOKI}/loki/api/v1/query_range" \
    --data-urlencode "query={service_name=\"${service}\"} | trace_id=\"${trace_id}\"" |
    jq -e '.data.result | length > 0'
}
prom_has_metric() {
  curl -fsSG "${PROM}/api/v1/query" --data-urlencode "query=smoke_gauge{job=\"${service}\"}" |
    jq -e '.data.result | length > 0'
}
grafana_datasource_ok() {
  curl -fsS "${GRAFANA}/api/datasources/uid/$1/health" | jq -e '.status == "OK"'
}

expect "trace chegou ao Tempo" tempo_has_trace
expect "log chegou ao Loki (com trace_id como metadado estruturado)" loki_has_log
expect "métrica chegou ao Prometheus" prom_has_metric
for uid in prometheus tempo loki; do
  expect "Grafana: datasource ${uid} saudável" grafana_datasource_ok "${uid}"
done
echo "pipeline de observabilidade OK"
