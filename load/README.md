# Testes de carga (k6)

## `ledger-baseline.js`: linha de base do ledger (Fase 1)

Gatilho **gate de desempenho** da seção 7.4 do plano: os thresholds do k6 quebram o CI (job `load-baseline`).
Não é teste de estresse (isso é a Fase 7): é a referência de "antes" e o alarme de regressão.

```bash
docker compose --profile core up -d --wait ledger   # Postgres, migrations e ledger
docker compose --profile load run --rm k6           # código 0 = passou; 99 = threshold falhou
```

Resumo no terminal e em `load/results/ledger-baseline.json` (fora do Git; no CI, vira artefato).

### Carga

- **Chegada constante** (modelo aberto, `constant-arrival-rate`): 40 iterações/s por 30 s. Num modelo fechado, um
  ledger lento receberia menos carga e esconderia a própria lentidão (*coordinated omission*).
- Mistura: 40% `GetBalance`, 20% `ListStatement`, 20% `Credit`, 20% `Debit`, em 20 contas criadas pelo próprio
  roteiro (CPFs válidos sorteados), com saldo de sobra: qualquer status diferente de `OK` é erro de verdade.
- Variar sem editar: `RATE=80 DURATION=60s docker compose --profile load run --rm k6`.

### Thresholds e de onde vieram

Medição local (Mac, Docker Desktop com 4 CPUs, só o profile `core`), 3 rodadas, em 12/11/2026:

| | p95 medido | p99 medido | Threshold p95 | Threshold p99 |
|---|---|---|---|---|
| Leitura (`op:read`) | 3,6 a 5,2 ms | 8 a 62 ms | < 25 ms | < 100 ms |
| Escrita (`op:write`) | 10,6 a 11,6 ms | 19 a 74 ms | < 50 ms | < 150 ms |

- **p99** (o que o plano pede) com folga de cerca de 2x sobre a pior rodada; **p95**, estável a pausas isoladas do
  ambiente, com folga de cerca de 5x: pega degradação gradual.
- `ledger_unexpected_errors` < 1% e `checks` > 99%: o "erro acima de 1%" do plano.
- `dropped_iterations == 0`: se o k6 não manteve a taxa, o teste não mediu o que diz medir.
- **Volume mínimo** (`ledger_requests{op:*}` >= 90% do esperado): um threshold sem amostras **passa** no k6. Sem esta
  regra, uma tag renomeada ou um roteiro quebrado deixariam o gate verde sem medir nada.

**A stack de observabilidade distorce a cauda no Mac**: com o profile `obs` no ar, o p99 de leitura subiu de cerca de
10 ms para cerca de 300 ms, em pausas globais (dezenas de chamadas lentas no mesmo décimo de segundo, inclusive
leituras sem lock), sinal de CPU disputada na VM, não de contenção no ledger. Meça sempre só com `core`, como o CI.

**Pendência**: os limites vêm da máquina local. Quando o CI rodar, recalibrar com as medições do runner (artefato
`k6-ledger-baseline`).

### Se o gate falhar

1. **Confirme que não é ruído**: rode de novo localmente, só com `core`. Falhou só uma vez e o p95 está normal? É pausa
   do ambiente; se repetir no CI, recalibre com dados (nunca afrouxe o limite sem medir).
2. **Erro acima de 1%**: veja os status no resumo e os logs do ledger (`docker compose logs ledger`).
   `UNAVAILABLE` em escrita = conta ocupada (`lock_timeout`); `INTERNAL` = bug, a causa está no log.
3. **Latência**: compare com o artefato do último CI verde. Suspeitos comuns: query nova sem índice (veja o plano
   com `EXPLAIN`), round-trips a mais por chamada, pool pequeno (`pgxpool.empty_acquire_wait_time` no dashboard
   do ledger).
4. **Testar que o gate dispara**: `docker compose --profile load run --rm -e P99_WRITE_MS=1 k6` deve sair com 99.
