# Testes com Postman — Wager Backend

Coleção e ambiente prontos para exercitar **tudo** em minutos, com asserções
automáticas (replay, conflito, reversões, isolamento, 404, 401/403).

## 1. Suba o ambiente

```sh
cp .env.example .env            # no Windows: copy .env.example .env
docker compose up -d            # Postgres, LocalStack, Keycloak + migrations
docker compose up --build -d    # API (:8081) e worker (3 réplicas)
```

Confirme: `powershell -ExecutionPolicy Bypass -File scripts/smoke-test.ps1` — se passar, a API está saudável.

## 2. Importe no Postman

1. **Import** → `docs/postman/wager-backend.postman_collection.json`
2. **Import** → `docs/postman/wager-backend.postman_environment.json`
3. Selecione o ambiente **“Wager Backend · Local”** no seletor (canto superior direito).

## 3. Rode tudo (Collection Runner)

- **Collection Runner** → selecione a coleção → **Run**.
- A ordem importa: a pasta `00 · Auth` preenche os tokens e cada request captura os
  IDs (`walletId`, `txId`, `betExternalId`) para os seguintes.
- Resultado esperado: **todas as asserções verdes** (o runner mostra o resumo).

> Os tokens do Keycloak expiram em **5 min**. Se rodar só uma request depois de
> muito tempo, refaça a pasta `00 · Auth` primeiro.

## 4. O que cada pasta cobre

| Pasta | Cobre |
|---|---|
| `00 · Auth` | `client_credentials` para `internal-service`, `provider-a`, `provider-b` |
| `01 · Health` | `/health/live` e `/health/ready` (públicos) |
| `02 · Carteira` | Abrir carteira (201), consulta, extrato com cursor, reconciliação |
| `03 · Operações` | BET, **replay** (`idempotentReplay=true`), **conflito 409**, WIN, LOSS (sem saldo/movimento), REFUND, **ROLLBACK duplicado** (rejeição), consultas, **404** |
| `04 · Autorização` | **403** de `provider-b` lendo transação do `provider-a`, **403** de provedor abrindo carteira, **401** sem token |

## 5. Cenários extras que valem demonstrar

- **Concorrência 80/80** (obrigatório do desafio): use o teste de integração —
  `go test -tags=integration -race ./internal/adapters/inbound/httpapi/ -v` — ou o
  `scripts/smoke-test.ps1`, que cobre o cenário.
- **Reversão antes da referência** (`PENDING_REFERENCE` → 202) e a retomada pelo
  worker: envie um `REFUND` com `referenceExternalTransactionId` inexistente
  (resposta `202`) e consulte a transação depois com `GET /wagering/transactions/{id}`.
- **Idempotência entre HTTP e SQS**: a mesma operação por SQS (fila
  `wager-transactions.fifo`) devolve o mesmo resultado do HTTP.

## Variáveis de ambiente usadas pela coleção

| Variável | Default |
|---|---|
| `baseUrl` | `http://localhost:8081` |
| `keycloakUrl` | `http://localhost:8080` |
| `realm` | `wager` |

Os *secrets* dos clientes são os de **teste local** (`dev-*-secret`), definidos no
realm em `deploy/keycloak/realm-export.json` — nunca use esses valores fora do ambiente local.
