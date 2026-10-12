# Testes de carga (k6)

> **Status honesto:** o script e a metodologia estão prontos e o ambiente sobe.
> **Os números de p50/p95/p99 não foram medidos** — sem uma execução real, publicar
> números seria inventá-los. O comando abaixo produz o relatório completo.

---

## Por que três cenários

| Cenário | Perfil | O que revela |
|---|---|---|
| `ramp_up` | Carga crescente até 30 VUs e patamar | O **joelho da curva**: a partir de qual carga a latência degrada |
| `spike` | Salto súbito para 60 VUs e queda | Se o sistema **se recupera** de uma rajada (pool saturado, fila acumulada) |
| `soak` | 10 VUs por 3 minutos | **Vazamentos**: goroutines, conexões que não voltam ao pool, memória |

Um sistema pode passar no `ramp_up` e falhar no `spike` — são perguntas diferentes.

---

## Pré-requisitos

Ambiente no ar e identidades de teste disponíveis:

```sh
docker compose up -d
docker compose build api worker && docker compose up -d
```

Confirme que a API responde antes de disparar carga:

```sh
curl -s http://localhost:8081/health/ready
```

---

## Execução

### Via container (sem instalar o k6)

```sh
# Windows (cmd)
docker run --rm -i --network host -v "%cd%/tests/k6":/scripts grafana/k6 run /scripts/bet-flow.js

# Linux/macOS
docker run --rm -i --network host -v "$PWD/tests/k6":/scripts grafana/k6 run /scripts/bet-flow.js
```

### Com o k6 instalado

```sh
k6 run tests/k6/bet-flow.js
```

### Cenário único

```sh
k6 run -e SCENARIO=ramp_up tests/k6/bet-flow.js
k6 run -e SCENARIO=spike   tests/k6/bet-flow.js
k6 run -e SCENARIO=soak    tests/k6/bet-flow.js
```

### Saída em JSON, para comparar execuções

```sh
k6 run --out json=resultado.json tests/k6/bet-flow.js
```

---

## Metodologia

1. **`setup()`** obtém os tokens reais no Keycloak (`client_credentials`) e cria
   **uma carteira por VU**. Carteiras separadas são essenciais: o lock é **por
   carteira**, então compartilhar uma só mediria serialização, não paralelismo.
2. **Cada VU usa a própria carteira**, com `Idempotency-Key` distinta por iteração —
   de modo que cada requisição seja uma operação **nova**, e não um replay.
3. **Saldo inicial alto** (`1.000.000,00`): o objetivo é medir latência, não a taxa de
   rejeição por saldo insuficiente.
4. **Pausa aleatória** entre iterações (0,2–0,7s) para não medir saturação do cliente.
5. O cenário `duplicates` (abaixo) é a exceção: ele **quer** medir idempotência sob
   concorrência.

---

## Thresholds

Definidos em `options.thresholds` — são **assertivas**: o k6 sai com código ≠ 0 quando
falham, o que os torna utilizáveis em CI.

| Threshold | Valor de partida | Razão |
|---|---|---|
| `http_req_failed` | `< 1%` | Praticamente zero é o esperado neste fluxo |
| `http_req_duration{scenario:ramp_up}` | `p(95) < 500ms`, `p(99) < 1000ms` | Regime sustentado |
| `http_req_duration{scenario:spike}` | `p(95) < 1000ms` | No pico toleramos mais latência; **erro, não** |
| `http_req_failed{scenario:spike}` | `< 2%` | |
| `http_req_duration{scenario:soak}` | `p(95) < 500ms` | Estabilidade |
| `http_req_failed{scenario:soak}` | `< 0,1%` | |

> **Ajuste após a primeira execução.** Estes números são ponto de partida para o
> ambiente local (LocalStack e PostgreSQL no mesmo host, disputando CPU com o k6).
> Threshold que nunca falha não protege nada; threshold que sempre falha é ignorado.

---

## Métricas de negócio

Além das métricas HTTP, o script publica contadores próprios:

| Métrica | Significado |
|---|---|
| `bets_processed` | Operações concluídas com sucesso |
| `bets_rejected` | Rejeições de negócio |
| `bets_idempotent_replay` | Reenvios reconhecidos como replay |
| `bet_balance_observed` | Distribuição do saldo retornado nas respostas |

---

## Cenário de idempotência sob carga

O cenário `duplicates` já está implementado no script (`duplicateFlow`) e dispara a
**mesma** operação em paralelo, verificando que **apenas uma** movimenta o saldo. Para
usá-lo, adicione o cenário às `options` (ou execute a função diretamente):

```sh
k6 run -e SCENARIO=duplicates -e WALLETS=20 tests/k6/bet-flow.js
```

**Limitação registrada:** o k6 usa um único processo e um único cliente HTTP por VU. A
prova cabal de concorrência entre **processos independentes** está no teste Go
`TestConcurrentDuplicateBets` (50 goroutines contra um servidor HTTP real) e no
`deploy.replicas: 3` do worker — que são executados contra o mesmo banco.

---

## Como ler o relatório

O que importa, na ordem:

1. **`http_req_failed`** — se subiu, pare aqui: o resto dos números não significa nada.
2. **`http_req_duration` p95/p99** — a distribuição, não a média. Uma média boa
   convive com um p99 péssimo.
3. **`bets_processed` vs. `bets_rejected`** — se `rejected` crescer, a carga está
   medindo rejeição, não processamento.
4. **`vus` e `iterations`** — confirmam que a carga pretendida realmente aconteceu.
5. **Atraso da outbox**: consulte as métricas da aplicação durante o teste —
   `wager_outbox_lag_seconds` e `wager_outbox_relay_total`:

```sh
curl -s http://localhost:9090/metrics | findstr wager_outbox
```

---

## O que este teste NÃO é

Não é um benchmark de throughput absoluto. LocalStack e PostgreSQL rodam no mesmo host
do k6, competindo por CPU. Os números servem para comparar **execuções entre si**
(regressão), não para prever produção.
