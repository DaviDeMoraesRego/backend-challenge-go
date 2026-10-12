# Processamento Distribuído de Apostas — Go

Serviço de processamento de operações financeiras (BET, WIN, LOSS, REFUND, ROLLBACK)
sobre carteiras de jogadores, com **integridade absoluta**, **concorrência segura por
carteira**, **idempotência persistente** e **recuperação de falhas com semântica
at-least-once**.

> Decisões de arquitetura, alternativas rejeitadas e limitações assumidas estão em
> [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## Sumário

- [Pré-requisitos](#pré-requisitos)
- [Subida rápida](#subida-rapida)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Filas SQS](#filas-sqs)
- [Migrations](#migrations)
- [Identidades de teste (Keycloak)](#identidades-de-teste-keycloak)
- [Chamadas de exemplo](#chamadas-de-exemplo)
- [Códigos HTTP](#códigos-http)
- [Códigos de falha](#códigos-de-falha)
- [Testes](#testes)
- [Observabilidade](#observabilidade)
- [Encerramento gracioso](#encerramento-gracioso)
- [Estrutura do repositório](#estrutura-do-repositório)
- [Solução de problemas](#solução-de-problemas)

---

## Pré-requisitos

| Ferramenta | Versão usada | Observação |
|---|---|---|
| Go | **1.27.1** (declarada em [`go.mod`](go.mod) e no [`Dockerfile`](Dockerfile)) | |
| Docker | 29.x | Com Compose v2 |
| PowerShell | 5.1+ | Apenas para o script de fumaça |
| MinGW-w64 (gcc) | opcional | **Necessário somente para `go test -race` no Windows** (ver [Testes](#testes)) |

Nenhuma ferramenta de banco, broker ou CLI da AWS é necessária no host: tudo roda em
containers.

---

## Subida rápida

```sh
# 1) Variáveis de ambiente locais (o arquivo tem valores de exemplo, sem segredos reais)
cp .env.example .env        # no Windows: copy .env.example .env

# 2) Banco, LocalStack, Keycloak e migrations
docker compose up -d

# 3) API + 3 réplicas de worker
docker compose build api worker
docker compose up -d

# 4) Verificação ponta a ponta (autenticação real + cenário de concorrência + reconciliação)
powershell -ExecutionPolicy Bypass -File scripts/smoke-test.ps1
```

O comando equivalente ao exigido pelo desafio — sobe TUDO, incluindo as imagens:

```sh
docker compose up --build
```

Serviços e portas:

| Serviço | URL no host | Observação |
|---|---|---|
| API | http://localhost:8081 | A 8080 no host é do Keycloak |
| Métricas | http://localhost:9090/metrics | Endpoint Prometheus |
| Keycloak | http://localhost:8080 | Admin: `admin` / `admin` |
| LocalStack (SQS) | http://localhost:4566 | Filas provisionadas automaticamente |
| PostgreSQL | localhost:5432 | `wager` / `wager` |

---

## Variáveis de ambiente

Todas documentadas em [`.env.example`](.env.example). As essenciais:

| Variável | Usada por | Descrição |
|---|---|---|
| `DATABASE_URL` | API e worker | String de conexão do PostgreSQL |
| `SQS_TRANSACTIONS_QUEUE_URL` | API e worker | Fila FIFO de entrada |
| `SQS_EVENTS_QUEUE_URL` | Worker | Destino dos eventos de integração |
| `AWS_ENDPOINT_URL` | API e worker | LocalStack. **Vazio em produção** faz o SDK usar a AWS real |
| `KEYCLOAK_ISSUER` | API | Emissor **validado** no token (`iss`) |
| `KEYCLOAK_JWKS_URL` | API | Endpoint de onde as chaves públicas são buscadas |
| `METRICS_ADDR` | API e worker | Endereço do servidor de métricas |
| `HTTP_READ_TIMEOUT` / `HTTP_WRITE_TIMEOUT` / `HTTP_IDLE_TIMEOUT` | API | Timeouts do servidor HTTP (defaults `15s` / `30s` / `60s`) |
| `SHUTDOWN_TIMEOUT` | API e worker | Prazo do encerramento gracioso |

> **`KEYCLOAK_ISSUER` ≠ `KEYCLOAK_JWKS_URL`.** O `iss` do token é o endereço que o
> *cliente* usa (`http://localhost:8080/...`), enquanto o JWKS é obtido pelo endereço
> *interno* da rede do Compose (`http://keycloak:8080/...`). Derivar um do outro
> causaria "issuer mismatch" — por isso são variáveis separadas.

Configuração inválida **impede a subida**, com a lista do que falta:

```
configuracao invalida error="config: variaveis de ambiente obrigatorias ausentes: DATABASE_URL, SQS_TRANSACTIONS_QUEUE_URL (veja .env.example)"
```

---

## Filas SQS

Provisionadas automaticamente pelo LocalStack no [`deploy/localstack/init/ready.d/01-create-queues.sh`](deploy/localstack/init/ready.d/01-create-queues.sh):

| Fila | Papel |
|---|---|
| `wager-transactions.fifo` | Entrada de operações |
| `wager-transactions-dlq.fifo` | DLQ da entrada (`maxReceiveCount=5`) |
| `wager-events.fifo` | Eventos de integração publicados pela outbox |
| `wager-events-dlq.fifo` | DLQ dos eventos |

Verificar:

```sh
docker compose exec localstack awslocal sqs list-queues
docker compose exec localstack awslocal sqs get-queue-attributes \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo --attribute-names All
```

---

## Migrations

Ficam em [`migrations/`](migrations/) e são aplicadas por um serviço one-shot do Compose
(a API e o worker só sobem com `condition: service_completed_successfully`). O schema é
versionado: `0001_init` cria tabelas/constraints/triggers; `0002_outbox_dead_letter`
adiciona a marcação `dead_at` do outbox, que retira do polling os eventos que esgotaram
as tentativas (dead-letter sem apagar a linha, preservando a auditoria).

```sh
# Aplicar (idempotente: reexecutar não altera nada quando o schema está atualizado)
docker compose run --rm migrate -path=/migrations \
  -database="postgres://wager:wager@postgres:5432/wager?sslmode=disable" up

# Reverter TUDO (usa o .down.sql)
docker compose run --rm migrate -path=/migrations \
  -database="postgres://wager:wager@postgres:5432/wager?sslmode=disable" down -all

# Versão atual do schema
docker compose run --rm migrate -path=/migrations \
  -database="postgres://wager:wager@postgres:5432/wager?sslmode=disable" version
```

Com [`make`](Makefile): `make migrate-up`, `make migrate-down`, `make migrate-version`.

---

## Identidades de teste (Keycloak)

O realm `wager` é importado de [`deploy/keycloak/realm-export.json`](deploy/keycloak/realm-export.json)
(veja também [`deploy/keycloak/README.md`](deploy/keycloak/README.md)).

| Client | Escopo | Usa para |
|---|---|---|
| `internal-service` | `internal` | Abrir carteira, consultar/extrato, reconciliar |
| `provider-a` | `provider-a` | Enviar operações e ler as **próprias** transações |
| `provider-b` | `provider-b` | Testes de isolamento entre provedores |

```sh
curl -s -X POST http://localhost:8080/realms/wager/protocol/openid-connect/token \
  -d grant_type=client_credentials -d client_id=provider-a -d client_secret=dev-provider-a-secret
```

O token traz `service_scope` e `provider_id`. **É daí que a API tira o provedor
autorizado** — nunca do corpo da requisição.

---

## Chamadas de exemplo

Substitua `$INTERNAL` e `$PROVIDER_A` pelos tokens acima.

### Abrir carteira (serviço interno)

```sh
curl -s -X POST http://localhost:8081/wallets \
  -H "Authorization: Bearer $INTERNAL" -H "Content-Type: application/json" \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

```json
{"id":"0192f291-27dd-7d3f-8071-5f8685deef37","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","balance":{"amount":"1000.00","currency":"BRL"},"version":1}
```

### Enviar operação (provedor) — `Idempotency-Key` é obrigatório

```sh
curl -s -X POST http://localhost:8081/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H "Content-Type: application/json" \
  -H "Idempotency-Key: provider-a:transaction-123" \
  -d '{
        "providerId":"provider-a",
        "externalTransactionId":"transaction-123",
        "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
        "walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
        "roundId":"round-987",
        "gameId":"fortune-chimp",
        "kind":"BET",
        "money":{"amount":"25.00","currency":"BRL"}
      }'
```

```json
{"transactionId":"0192f298-345e-7e38-af88-e43f851a819d","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}
```

Reenviar a **mesma** chave com o **mesmo** conteúdo devolve o resultado original com
`"idempotentReplay": true` — e o saldo **do processamento original**. Mesma chave com
conteúdo diferente devolve **409**.

Para reversões, acrescente `"referenceExternalTransactionId":"transaction-123"` e use
`"kind":"REFUND"` (devolve uma BET) ou `"kind":"ROLLBACK"` (desfaz BET, WIN ou REFUND).

### Consultas

```sh
curl -s "http://localhost:8081/wallets/$WALLET_ID"                     -H "Authorization: Bearer $INTERNAL"
curl -s "http://localhost:8081/wallets/$WALLET_ID/ledger?limit=50"     -H "Authorization: Bearer $INTERNAL"
curl -s "http://localhost:8081/wallets/$WALLET_ID/ledger?cursor=$NEXT" -H "Authorization: Bearer $INTERNAL"
curl -s "http://localhost:8081/wagering/transactions/$TX_ID"           -H "Authorization: Bearer $PROVIDER_A"
curl -s "http://localhost:8081/providers/provider-a/wagering/transactions/transaction-123" -H "Authorization: Bearer $PROVIDER_A"

curl -s -X POST "http://localhost:8081/wallets/$WALLET_ID/reconciliation" -H "Authorization: Bearer $INTERNAL"
```

O `cursor` do extrato é **opaco** (base64) e nunca deve ser interpretado pelo cliente —
apenas devolvido na chamada seguinte.

---

## Códigos HTTP

| Código | Situação |
|---:|---|
| 200 | Operação concluída (PROCESSED **ou** REJECTED — rejeição é resultado válido) |
| 201 | Carteira criada |
| 202 | Operação registrada aguardando referência (`PENDING_REFERENCE`) |
| 400 | Corpo inválido, campo desconhecido ou `Idempotency-Key` ausente |
| 401 | Credencial ausente ou inválida |
| 403 | Identidade sem permissão para o recurso |
| 404 | Recurso inexistente |
| 409 | Conflito de idempotência / unicidade |
| 422 | Entrada válida em forma, inválida em conteúdo (regra de negócio de entrada) |
| 500 | Falha inesperada |
| 503 | Dependência indisponível (banco, broker ou IdP) — **retry** |

---

## Códigos de falha

Estáveis e documentados (definidos em [`internal/domain/transaction/failure.go`](internal/domain/transaction/failure.go)):

| `failureCode` | Corrigível? | Quando ocorre |
|---|---|---|
| `BET_INSUFFICIENT_FUNDS` | Não | Aposta sem saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | Não | Reversão exigiria débito acima do saldo (**código distinto** do de aposta) |
| `REFERENCE_NOT_FOUND` | Sim | Referência não apareceu dentro do prazo de tentativas |
| `REFERENCE_NOT_PROCESSED` | Não | A referência existe, mas terminou sem sucesso |
| `REFERENCE_STILL_PENDING` | Sim | A referência ainda está pendente (continua aguardando) |
| `DUPLICATE_REVERSAL` | Não | A referência já recebeu uma reversão bem-sucedida |
| `REFERENCE_MISMATCH` | Sim | Operação e referência discordam em jogador/carteira/rodada/moeda |
| `REFERENCE_KIND_NOT_ALLOWED` | Não | Ex.: `REFUND` de um `WIN` |
| `AMOUNT_MISMATCH` | Sim | Valor da reversão difere do referenciado |
| `INVALID_AMOUNT` / `INVALID_KIND` | Sim | Valor fora da política do tipo / tipo desconhecido |
| `CURRENCY_MISMATCH` | Sim | Moeda da operação difere da carteira |
| `WALLET_NOT_FOUND` | Não | Carteira **existe**, mas pertence a outro jogador (resposta `REJECTED` para não vazar a existência). Carteira inexistente em si vira **404** |
| `IDEMPOTENCY_CONFLICT` | Não | Chave reutilizada com conteúdo diferente |
| `INTERNAL_ERROR` | Não | Falha permanente de infraestrutura, registrada para auditoria |

---

## Testes

```sh
# Unitários (herméticos: sem Docker, sem rede) — domínio + casos de uso com fakes
go test ./...

# Com o detector de races (exige cgo + compilador C; ver nota abaixo)
go test -race ./...

# Análise estática e formatação
go vet ./...
gofmt -l .

# Integração: PostgreSQL REAL (usa o container do compose)
go test -tags=integration ./internal/adapters/outbound/postgres/ -v

# Bateria de concorrência: HTTP real + Postgres real, com -race
go test -tags=integration -race ./internal/adapters/inbound/httpapi/ -v

# Tudo de uma vez
go vet -tags=integration ./...
```

### Nota sobre `-race` no Windows

O detector de races exige **cgo** e um compilador C. Se `go test -race` falhar com
`cgo: C compiler "gcc" not found`:

```cmd
winget install --id BrechtSanders.WinLibs.POSIX.UCRT -e
go env -w CGO_ENABLED=1
:: reinicie o VS Code / terminal para o PATH atualizar
```

Sem compilador C instalado, o alvo `make test-race-docker` roda os testes com `-race`
dentro de um container `golang`, sem toolchain local.

### O que cada suíte cobre

| Suíte | Cobre |
|---|---|
| `internal/domain/...` | Money (parsing, escala, overflow, moedas), invariantes da carteira, máquina de estados, regras dos 5 tipos, ledger (cadeia de saldo), eventos |
| `internal/application/usecase` | Autorização, idempotência/replay/conflito, reversões, `PENDING_REFERENCE`, cursor, reconciliação, relay e worker de referências (com fakes) |
| `.../postgres` (integração) | Constraints, `FOR UPDATE`, atomicidade, ledger append-only (trigger), inbox, lease do outbox, rollback em erro e em panic |
| `.../httpapi` (integração) | **Concorrência**: 50 envios duplicados em paralelo, disputa 80/80 sobre 100, carteiras distintas em paralelo, consistência ledger × saldo |

---

## Observabilidade

- **Logs**: JSON em stdout, com `correlationId` em toda requisição (o cliente pode
  enviar `X-Correlation-ID`, que é preservado e devolvido). Nunca registramos credenciais
  nem payload financeiro completo.
- **Métricas**: `http://localhost:9090/metrics` — `wager_http_request_duration_seconds`
  (latência), `wager_operations_total` (resultado por tipo/status),
  `wager_idempotent_replays_total` (duplicatas), `wager_idempotency_conflicts_total`,
  `wager_outbox_relay_total` (retries/falhas), `wager_outbox_lag_seconds`,
  `wager_pending_references_total` e `wager_reconciliation_divergences_total`.
- **Health**: `GET /health/live` (processo) e `GET /health/ready` (PostgreSQL + SQS).
  As rotas de health são **públicas** — um orquestrador precisa distinguir "processo
  morto" de "IdP fora", e as ações são opostas.

```sh
docker compose exec api wget -qO- http://localhost:9090/metrics | head -40  # se disponível
powershell -NoProfile -Command "(Invoke-WebRequest http://localhost:9090/metrics).Content" | Select-String wager_
```

---

## Encerramento gracioso

`docker compose stop api` (ou `docker kill -s TERM`) dispara:

1. parada de novas entradas no servidor HTTP / nos laços do worker;
2. conclusão do trabalho em andamento (requisições e mensagens);
3. fechamento do pool de conexões **depois** dos componentes que o usam;
4. saída com código 0 dentro de `SHUTDOWN_TIMEOUT`.

O Fx executa os hooks em ordem inversa à de registro — é por isso que o pool é
registrado primeiro e fechado por último.

---

## Estrutura do repositório

```
cmd/                     Binários: api e worker
deploy/                  Provisionamento (Keycloak realm, init das filas SQS)
docs/                    ARCHITECTURE.md (decisões/ADRs); postman/ (coleção de testes)
internal/
  domain/                Regras puras: money, wallet, transaction, ledger, event
  application/           ports (contratos) e usecase (orquestração)
  adapters/
    inbound/             httpapi (API + middlewares), sqsworker (consumidor SQS)
    outbound/            postgres, sqs (publisher), keycloak (JWKS)
  platform/              config, clock, idgen, metrics, observability, awsclient
  bootstrap/             Wiring do Fx (o composition root)
migrations/              Schema versionado (golang-migrate)
scripts/                 smoke-test.ps1 (verificação ponta a ponta)
tests/k6/                Teste de carga
```

---

## Solução de problemas

| Sintoma | Causa provável | Correção |
|---|---|---|
| `error during connect: dial tcp ... 2375` | `DOCKER_HOST` apontando para um socket inexistente | Remover a variável de ambiente ou apontar para o named pipe do Docker Desktop |
| API reinicia em loop com `missing types: ports.X` | Provider do Fx faltando (`fx.As`) | Ver `docs/ARCHITECTURE.md`, ADR sobre o wiring |
| `401` em todas as chamadas | Token expirado (5 min) ou `KEYCLOAK_ISSUER` divergente do `iss` | Renovar o token; conferir `KC_HOSTNAME` do Keycloak |
| `503` no `/health/ready` | PostgreSQL ou LocalStack fora | `docker compose ps` e `docker compose logs` |
| Operação presa em `PENDING_REFERENCE` | A transação referenciada não chegou | É o comportamento esperado; o worker retenta com backoff até o teto, quando rejeita com `REFERENCE_NOT_FOUND` |
| Mensagens na DLQ | Mensagem inválida ou tentativas esgotadas | `docker compose exec localstack awslocal sqs receive-message --queue-url .../wager-transactions-dlq.fifo` |
