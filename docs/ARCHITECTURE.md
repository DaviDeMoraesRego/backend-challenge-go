# ARCHITECTURE — Decisões, Alternativas e Limitações

Documento de decisões do serviço de processamento de apostas. Cada decisão está
registrada como **ADR** (Architecture Decision Record), no formato
**Contexto → Decisão → Alternativas consideradas → Consequências**.

> Este arquivo responde "por que assim?" — o **como** está no código, com
> comentários de intenção em cada ponto relevante.

---

## Visão geral

```
                ┌──────────────────────────────────────────┐
   HTTP ────────►  adapters/inbound/httpapi                 │
                │    (middlewares: recover, correlation,    │
                │     log, metrics, auth/JWKS)              │
                └───────────────────┬──────────────────────┘
                                    │ chama PORTAS INBOUND
                ┌───────────────────▼──────────────────────┐
   SQS  ────────►  adapters/inbound/sqsworker                │
                │    (long polling, inbox, decisão de DLQ)  │
                └───────────────────┬──────────────────────┘
                                    │
        ┌───────────────────────────▼────────────────────────────┐
        │  application/usecase                                    │
        │    PlaceWagerOperation · OpenWallet · ReconcileWallet   │
        │    RelayOutbox · ResolvePendingReferences               │
        └───────────────────────────┬────────────────────────────┘
                                    │ depende de PORTAS OUTBOUND
        ┌───────────────────────────▼────────────────────────────┐
        │  domain  (Money · Wallet · Transaction · Ledger · Event)│
        │  zero dependências externas                             │
        └───────────────────────────┬────────────────────────────┘
                                    │ implementadas por
        ┌───────────────────────────▼────────────────────────────┐
        │  adapters/outbound: postgres · sqs · keycloak          │
        └────────────────────────────────────────────────────────┘
```

**API** e **worker** são binários separados (`cmd/api`, `cmd/worker`) com o mesmo
núcleo. O worker roda três laços: consumidor SQS, relay do outbox e worker de
referências pendentes.

---

## ADR-001 — Orquestração: Go puro + SQS + Outbox, **sem** Temporal

**Contexto.** A máquina de estados, os retries com backoff, a resolução de
referências pendentes e o relay do outbox poderiam ser orquestrados pelo Temporal.
A avaliação foi pedida explicitamente.

**Decisão.** **Não usar Temporal.** Usar workers SQS, outbox transacional e máquina
de estados validada em código.

**Comparação objetiva.**

| Critério | Temporal | Go puro + SQS + Outbox (escolhido) |
|---|---|---|
| Complexidade operacional | +1 cluster (4 serviços: server, UI, DB próprio), versionamento de workflows, mais um modelo de observabilidade | Zero serviços novos: usa exatamente PostgreSQL + SQS, já exigidos pelo desafio |
| Aderência ao escopo | **Redundante**: Temporal substitui fila, retries e estado durável — que são justamente os itens cobrados (§10/§11) | Implementa as primitivas exigidas: worker SQS, inbox/outbox, visibility timeout, DLQ, replay |
| Curva de aprendizado | Workflow/activity/signal/schedule + determinismo + replay + versionamento. Alto custo antes da primeira linha útil | Todo o ferramental é Go: `SELECT ... FOR UPDATE`, `LISTEN`, goroutines, context |
| Testabilidade | `testsuite` própria, com restrições de determinismo (nada de I/O fora de activity) | `go test`, `-race`, testcontainers e a API real — sem abstração nova para testar |
| Risco de overengineering | **Alto** para 5 estados e 5 tipos de operação | Baixo: cada componente tem justificativa e teste |
| Onde Temporal ganharia | Sagas longas entre vários serviços, timers de dias, replay de histórico, visibilidade nativa de execuções em andamento | — (backoff fica com `next_attempt_at` + redrive para DLQ) |

**Consequências.**
- Abrimos mão de **replay de histórico** e de uma UI de execuções em andamento. Em
  troca, o rastro é reconstruível a partir de `wager_transactions` + `outbox_events`
  (ambas auditáveis).
- Ganhamos: uma peça a menos para operar, um modelo mental a menos para manter, e
  aderência literal ao que o desafio pede.
- **Ponto de reversão**: se um dia surgir uma saga entre múltiplos serviços
  (ex.: settlement com adquirente externo), o caso de uso já está isolado atrás de
  portas — trocar a orquestração não exige tocar no domínio.

---

## ADR-002 — Dinheiro: `int64` em unidades mínimas, escala fixa 2

**Contexto.** Dinheiro não pode passar por `float` em nenhuma etapa (§5.1).

**Decisão.** `Money` é um Value Object imutável com `int64` em centavos + moeda ISO
4217 embutida. Escala fixa de 2 casas, sem arredondamento silencioso.

**Alternativas consideradas.**

| Alternativa | Por que não |
|---|---|
| `float64` | Proibido pelo §5.1. Além disso, `0.1 + 0.2 != 0.3` — o erro é binário, não de arredondamento |
| `string` decimal | Exato, porém força conversão a cada operação e não dá garantia de tipo |
| Biblioteca decimal (`shopspring/decimal`) | Precisão arbitrária e custo de alocação em hot path para uma escala que **é fixa** (2 casas). Não resolvia nenhum problema que `int64` não resolvesse |

**Consequências.**
- Parsing estrito: rejeita vazio, `NaN`, `Infinity`, notação científica, escala > 2,
  sinal em entrada externa e whitespace.
- Overflow é tratado **explicitamente** em parsing, soma, subtração e negação —
  inclusive o caso especial `MinInt64`, cujo oposto não é representável.
- Limite documentado: ±2^63 centavos (≈ ±92 quatrilhões de reais).
- Persistência em `BIGINT`; transporte como **string** (`{"amount":"25.00"}`), nunca
  como número JSON — assim nenhum consumidor interpreta como `float`.

---

## ADR-003 — Ledger: lançamento único com cadeia de saldo (double-entry rejeitado)

**Contexto.** O desafio define `WalletLedgerEntry` com `direction`, `balanceBefore` e
`balanceAfter`, unicidade `(walletId, transactionId)` e imutabilidade — e afirma que
**partidas dobradas são opcionais**. O prompt inicial do usuário pedia double-entry
como obrigatório; o spec oficial (fonte de avaliação) prevalece.

**Decisão.** Ledger **append-only de lançamento único**, com a invariante de cadeia
`balanceAfter = balanceBefore ± amount` validada no domínio **e** no banco.

**Alternativa considerada: partidas dobradas.** Rejeitada como mecanismo primário
porque:
1. o spec exige que **LOSS não produza lançamento** e que operações rejeitadas não
   produzam lançamento — um modelo de partidas dobradas tenderia a exigir o par
   contábil em toda operação;
2. o ledger do spec já é auditável e **reconciliável**: somar créditos e subtrair
   débitos reconstrói o saldo;
3. introduziria uma dimensão (plano de contas) sem requisito correspondente.

**Consequências.**
- `LOSS` e rejeições **não** geram lançamento (§6.4) — e isso é verificado por teste
  (`checkedEntries` = 4 após 4 operações, sendo uma LOSS).
- Imutabilidade imposta por **TRIGGER** (UPDATE/DELETE/TRUNCATE) e por `REVOKE`. O
  teste de integração prova que as três operações são recusadas pelo banco.
- A reconciliação (§9) reconstrói o saldo e **nunca corrige**: correção é um novo
  lançamento.
- **Custo assumido**: se o sistema evoluir para contabilidade por contas (receita da
  casa, passivo do jogador), o ledger precisará do par dobrado. A estrutura
  (`direction`, `balanceBefore/After`) é compatível com a migração.

---

## ADR-004 — Concorrência: lock pessimista **por carteira** + FIFO como ordenação

**Contexto.** O §8 exige coordenação **por carteira**, proíbe locks globais, exige
ausência de lost updates — e determina que as invariantes valham no banco,
independentemente de locks de aplicação ou da deduplicação do SQS FIFO.

**Decisão.** Três camadas, com papéis distintos:

1. **`SELECT ... FROM wallets WHERE id = $1 FOR UPDATE`** — serializa as escritas da
   carteira, em qualquer processo. É a coordenação real.
2. **Constraints do schema** — `CHECK (balance_minor >= 0)`, unicidade do ledger,
   unicidade da chave de idempotência, índice único parcial de reversão. São o
   *backstop* para o caso de um bug contornar o lock.
3. **SQS FIFO com `MessageGroupId = walletId`** — apenas **ordenação** da entrada
   assíncrona. Nenhuma invariante depende dele.

**Alternativas consideradas.**

| Alternativa | Por que não |
|---|---|
| Lock otimista com `version` + retry | Exige laço de retry em todo caminho; sob disputa alta, degrada em cascata. `version` aqui é auditoria, não token de concorrência (§6.2 diz que ela só incrementa em mudança de saldo) |
| `pg_advisory_xact_lock(hash(walletId))` | Funciona, mas acopla a serialização a um lock de sessão, invisível no `EXPLAIN` e mais difícil de depurar do que um lock de linha |
| Mutex em memória | **Não funciona** — o §8 exige múltiplos processos independentes, e memória não é compartilhada |
| Lock global | Proibido pelo §5.6 |

**Consequências.**
- Carteiras diferentes avançam em paralelo (medido: **8 carteiras em 109 ms**).
- Custo: escritas na **mesma** carteira são serializadas — que é exatamente o
  comportamento necessário para não estourar o saldo.
- O `UPDATE ... WHERE id = $1 AND version < $3` adiciona uma segunda defesa: um
  agregado obsoleto usado sem lock não sobrescreve o saldo (vira `ErrNotFound`).
- **Ordem de aquisição de locks é consistente** (carteira antes de transação) em
  todos os caminhos — o que evita deadlock entre o consumidor e o worker.

---

## ADR-005 — HTTP síncrono, sem commit intermediário de aceite

**Contexto.** O §6.3 permite concluir operações sem dependências de forma síncrona,
"sem commit intermediário de aceite".

**Decisão.** `POST /wagering/transactions` processa na mesma transação SQL: aceite,
efeitos (saldo + ledger + transação) e eventos no **mesmo commit**. Não existe estado
`PENDING` "de passagem" gravado para operações sem dependência.

**Alternativas consideradas.**
- **202 + processamento assíncrono sempre**: adiciona um estado intermediário, obriga
  o cliente a fazer *polling* e não é o que o spec pede para operações simples.
- **Fila só para operações dependentes**: possível, mas cria dois caminhos de entrada
  com garantias diferentes — e o §10 exige que HTTP e SQS compartilhem o caso de uso.

**Consequências.**
- `BET`/`WIN`/`LOSS` respondem `200` com o resultado **definitivo** (PROCESSED ou
  REJECTED).
- Reversão cuja referência não existe responde `202` com `status:
  PENDING_REFERENCE` — o único caso em que o aceite não é terminal.
- O caminho SQS usa **o mesmo caso de uso**, com o campo de inbox preenchido. Não há
  duas implementações para divergir.

---

## ADR-006 — Idempotência: chave + hash canônico, replay do saldo **original**

**Contexto.** §9: chave e conteúdo iguais → replay; chave igual e conteúdo diferente →
conflito; `(providerId, externalTransactionId)` não pode ser reaplicado com outra
chave; o replay de operação concluída devolve **o saldo observado no processamento
original**.

**Decisão.**
- `Idempotency-Key` obrigatório; o servidor **não substitui** uma chave recebida.
- **Hash SHA-256 de JSON canônico** (chaves ordenadas) dos campos de negócio,
  **excluindo** a chave de idempotência e metadados de transporte. A função é única
  para HTTP e SQS → a mesma operação gera o mesmo hash nos dois canais.
- Normalização **antes** do hash (`"25"`, `"25.0"` e `"25.00"` → mesmo hash),
  documentada e testada.
- `UNIQUE(idempotency_key)` **e** `UNIQUE(provider_id, external_transaction_id)`.
- O saldo do replay vem de `result_balance_minor`, **persistido no momento do
  processamento** — não de uma nova consulta à carteira.

**Consequências.**
- Replay devolve o saldo original mesmo que a carteira tenha mudado depois (testado).
- Dois pedidos simultâneos com a mesma chave: um insere, o outro perde a corrida no
  índice único e **relê o vencedor**, devolvendo replay — sem laço de retry visível ao
  cliente (testado com 50 chamadas paralelas).
- O hash é um contrato de formato: um teste *golden* congela os bytes canônicos, de
  modo que reordenar campos falha **antes** de quebrar a idempotência de operações já
  persistidas.

---

## ADR-007 — Outbox: lease, não lock de longa duração

**Contexto.** §11 exige múltiplos publishers, disputa por registros, backoff,
recuperação de trabalho abandonado e republicação preservando o `eventId`.

**Decisão.** `ClaimPending` faz, em UMA transação curta:
`SELECT ... FOR UPDATE SKIP LOCKED` → atualiza `next_attempt_at = agora + lease` →
**commit**. A publicação acontece **fora** de qualquer transação.

**Alternativas consideradas.**
- **Publicar dentro da transação** (mantendo o lock): prenderia uma linha do banco por
  todo o tempo de rede e, se o processo morresse, o lock só seria liberado no timeout
  do servidor.
- **Tabela de controle adicional** (`outbox_inflight`): mais estado para reconciliar —
  o lease resolve com uma coluna que já existia.

**Consequências.**
- **Duas janelas de falha, ambas tratadas:**
  - morrer após o commit e antes de publicar → o evento continua pendente e outra
    instância assume (nada se perde);
  - morrer após publicar e antes de confirmar → o evento será **republicado com o
    mesmo `eventId`** (testado), e o consumidor deduplica.
- Trabalho abandonado é recuperado por **vencimento do lease**, sem intervenção.
- Ao esgotar tentativas, o evento é **marcado como dead-letter** (`outbox_events.dead_at`,
  migration `0002`): permanece auditável mas **deixa de ser reivindicado**. Sem isso, o
  relay o republicaria para sempre em cadência de backoff. A métrica
  `wager_outbox_relay_total{outcome="failed"}` sinaliza.

---

## ADR-008 — Autenticação e autorização: escopos e isolamento por provedor

**Contexto.** §2: autenticação **e** autorização obrigatórias; a identidade determina
o `providerId`; provedores acessam só as próprias transações, inclusive em replays;
operações de carteira são restritas ao serviço interno.

**Decisão.**
- Keycloak com `client_credentials` e **dois mundos de cliente**: `internal-service`
  (escopo `internal`) e `provider-a`/`provider-b` (escopo `provider` + `provider_id`).
- Middleware valida **RS256** com algoritmo **fixado**, emissor validado e `exp`
  exigido; chaves públicas obtidas por **JWKS com cache** e re-busca em `kid`
  desconhecida (rotação), com intervalo mínimo entre buscas.
- **`providerId` vem do token, nunca da URL ou do corpo.** A rota
  `/providers/{providerId}/...` tem seu parâmetro **validado contra a identidade**.
- Provedor não acessa endpoints de carteira (403); serviço interno acessa tudo.

**Alternativas consideradas.**
- Confiar no `providerId` do payload: um provedor poderia operar em nome de outro —
  falha eliminatória (§14).
- Sessões/usuários com senha: fora do escopo (§2).
- Biblioteca de JWKS: implementado localmente (~40 linhas) para deixar explícito o que
  é validado e manter o controle da política de refresh.

**Consequências.**
- Verificado ponta a ponta: `provider-b` recebe **403** ao ler transação do
  `provider-a`; provedor recebe **403** em `/wallets/...`; requisição sem token recebe
  **401**.
- `KEYCLOAK_ISSUER` (valor do `iss`, voltado ao cliente) e `KEYCLOAK_JWKS_URL`
  (endpoint interno) são **variáveis distintas** — ver README.
- Falha de infraestrutura do IdP devolve **503**, não 401: trocar credencial não
  resolveria, e 401 mandaria o cliente depurar o problema errado.

---

## ADR-009 — Referências pendentes: `PENDING_REFERENCE` com backoff e teto

**Contexto.** §3: uma reversão pode chegar **antes** da transação referenciada. §7:
persistir como `PENDING_REFERENCE`, tentar novamente com backoff exponencial (inclusive
após reinício), definir teto de tentativas e, ao esgotar, finalizar como `REJECTED`.

**Decisão.**
- Referência inexistente → `PENDING_REFERENCE`, com `next_retry_at` calculado por
  backoff exponencial (base configurável, teto de 1 hora) e `attempts` incrementado.
- Referência existente mas **ainda pendente** → continua aguardando, com código
  `REFERENCE_STILL_PENDING` (distinto de "não encontrada").
- Referência que terminou **sem sucesso** → rejeição **definitiva**
  (`REFERENCE_NOT_PROCESSED`): esperar seria esperar para sempre.
- Ao esgotar as tentativas → `REJECTED` com `REFERENCE_NOT_FOUND` + evento de rejeição.
- **Retomada durável**: `correlation_id` é persistido na transação, de modo que o
  rastro (§12) sobrevive à retomada.
- O worker **relê a transação DEPOIS** de travar a carteira. Sem isso, duas instâncias
  que listaram a mesma pendência aplicariam a reversão duas vezes.

**Consequências.**
- A operação **não** é perdida nem duplicada: fica registrada e auditável até o
  desfecho.
- A mensagem SQS de entrada pode ser **removida** assim que a pendência está
  persistida (§6.5) — o worker de referências assume a continuidade.
- Testado: resolução quando a referência chega; expiração por teto; crescimento
  exponencial do intervalo.

---

## ADR-010 — Reversões: uma bem-sucedida por referência

**Contexto.** §7: garantir que a referência não receba duas reversões bem-sucedidas,
documentar o tratamento de combinações `REFUND`/`ROLLBACK` sobre a mesma aposta.

**Decisão.**
- `REFUND` reverte **BET**; `ROLLBACK` reverte **BET, WIN ou REFUND**.
- Valor da reversão deve ser **idêntico** ao referenciado (parciais fora de escopo).
- Operação e referência devem concordar em **provedor, jogador, carteira, rodada e
  moeda**.
- **No máximo uma reversão bem-sucedida por referência**, garantida por **índice único
  parcial** `(provider_id, reference_external_transaction_id) WHERE kind IN
  ('REFUND','ROLLBACK') AND state = 'PROCESSED'`. A checagem em código existe para
  devolver `DUPLICATE_REVERSAL` em vez de uma violação de constraint crua.
- Política de combinações: como `REFUND` e `ROLLBACK` compartilham a referência, um
  `REFUND` de uma BET **bloqueia** um `ROLLBACK` posterior da mesma BET (devolveria o
  débito duas vezes). Um `ROLLBACK` de um **REFUND** referencia o `REFUND` — logo, é
  permitido e fecha o ciclo com soma zero.
- Reversão que exigiria débito acima do saldo → `REVERSAL_INSUFFICIENT_FUNDS`,
  **código distinto** de `BET_INSUFFICIENT_FUNDS` (o provedor reage de forma diferente
  a "já gastou" versus "não tem saldo para apostar").

**Consequências.**
- O duplo estorno é impossível **no banco**, não apenas na aplicação.
- A ordem das verificações é fixa (tipo → conformidade → valor → duplicidade), o que
  torna a rejeição **reproduzível**: duas violações simultâneas sempre produzem o mesmo
  código.
- A mesma função de conformidade é usada pela entrada HTTP/SQS **e** pela retomada do
  worker — não há duas cópias para divergir.

---

## ADR-011 — Wiring com Fx: `fx.As`, módulos e ordem de encerramento

**Contexto.** §4 exige composição com `fx.Module`/`fx.Provide`/`fx.Invoke`, validação
na inicialização, cancelamento/prazos, encerramento com conclusão ou liberação do
trabalho em andamento e fechamento das dependências **depois** de quem as usa.

**Decisão.**
- Módulos por área (`observability`, `metrics`, `postgres`, `sqs-client`,
  `sqs-consumer`, `keycloak`, `usecases-core`, `usecases-worker`, `http`, `workers`).
- **`fx.As` em todo provider que devolve concreto mas é consumido por interface** —
  caso de todos os casos de uso, do `UnitOfWork`, do relógio, do gerador de ID e do
  publisher. O Fx **não** infere essa ligação.
- **Configuração carregada e validada no `main`** e injetada com `fx.Supply`: uma única
  leitura, um único conjunto de valores, e o `main` usa o mesmo `ShutdownTimeout` que a
  aplicação.
- **Ordem de encerramento**: o pool registra seu `OnStop` na **construção** (primeiro),
  então o Fx o executa **por último** — depois de HTTP e workers pararem. Registrar na
  ordem inversa produziria "pool is closed" durante o shutdown.
- Servidores fazem `net.Listen` explícito dentro do `OnStart` e **devolvem o erro**:
  porta ocupada impede a subida, em vez de deixar o processo "de pé" sem atender.

**Consequências.**
- Erros de grafo aparecem com o ponto exato e o tipo faltante
  (`missing type: ports.UnitOfWork (did you mean to Provide it?)`) — o que tornou a
  primeira subida diagnosticável em segundos.
- `SQSClientModule` entra nos **dois** processos (a API precisa do health check do
  broker; o worker, do cliente), enquanto `SQSConsumerModule` é exclusivo do worker. O
  Fx constrói apenas o que é pedido: o publisher não é instanciado na API.
- O worker não inclui `KeycloakModule` — não atende HTTP — e por isso as variáveis do
  IdP **não** são obrigatórias na validação de configuração dele.

---

## ADR-012 — Inbox: deduplicação que compartilha a transação do domínio

**Contexto.** §6.5: na entrada por SQS, o registro da inbox e a conclusão durável do
tratamento devem compartilhar a transação SQL das mudanças de domínio. §10: usar o
`messageId` do envelope como identidade durável e verificar o hash em reentregas.

**Decisão.**
- Tabela `inbox_messages` com PK `(consumer_name, message_id)` e `message_hash`.
- O `Register` **não** usa `ON CONFLICT DO NOTHING`: a violação de unicidade precisa
  chegar como `ErrAlreadyExists` para o caso de uso distinguir "mensagem nova" de
  "reentrega" e comparar o hash.
- `messageId` igual com conteúdo **diferente** → erro permanente (o adapter não remove a
  mensagem; o redrive a leva à DLQ).
- `MarkProcessed` roda dentro da transação do domínio — se o commit falhar, a marcação
  desaparece junto e a mensagem volta a ser tratada, com a idempotência da transação
  impedindo efeito duplo.

**Consequências.**
- A deduplicação por `messageId` é **separada** da idempotência financeira (chave de
  idempotência). São camadas complementares: uma protege o tratamento do transporte, a
  outra protege o dinheiro **mesmo** que a mensagem chegue por outro caminho.
- Uma referência pendente pode concluir a mensagem assim que a pendência está
  persistida (§6.5) — o worker assume.

---

## Interpretações adotadas

Onde o spec permitia mais de uma leitura, estas foram as escolhas (todas testadas):

1. **"Operações de carteira são restritas ao serviço interno"** → abrir carteira, ler
   carteira/extrato e **reconciliar** exigem escopo `internal`. Provedores usam apenas
   `/wagering/transactions` e `/providers/{providerId}/...`.
2. **"Pelo menos três processos independentes"** → atendido por `deploy.replicas: 3` do
   worker no Compose (três containers, com pools e memória próprios). Os testes de
   concorrência usam um servidor HTTP real com requisições paralelas de verdade.
3. **Integração com "containers reais"** → usamos os containers do **próprio
   `docker compose`** (`TEST_DATABASE_URL`), em vez de testcontainers-go. O requisito é
   "containers reais", e isso se cumpre; testcontainers adicionaria dependência e tempo
   de subida. **Consequência assumida**: os testes de integração truncam as tabelas do
   banco — em um projeto real, aponte `TEST_DATABASE_URL` para um banco dedicado.
4. **`REFUND`/`ROLLBACK` e "resultado financeiro retornado ao provedor"** → o saldo
   observado é persistido **apenas** quando houve resultado financeiro (PROCESSED). Em
   rejeição/`PENDING_REFERENCE`, o campo `balance` é **omitido** da resposta, em vez de
   devolver `0.00` (que o provedor interpretaria como saldo real).
5. **Rejeição não é erro de execução** → `REJECTED` responde **200** com `failureCode`;
   erros de transporte/validação usam 4xx/5xx. É o que permite ao worker **remover** a
   mensagem numa rejeição e **devolvê-la** numa falha transitória.

---

## Limitações e trabalho não concluído

| Item | Situação |
|---|---|
| **Tracing com OpenTelemetry** | **Não implementado.** É diferencial opcional do §12. O desenho está preparado: os handlers recebem `r.Context()` ao logar (pronto para um `slog.Handler` que injete `trace_id`), e o publisher já envia `correlationId`/`causationId` em **MessageAttributes** — o ponto exato por onde o `traceparent` viajaria |
| **Testes de carga com k6** | Script e metodologia em [`tests/k6/`](../tests/k6/README.md); **os números p50/p95/p99 não foram medidos** |
| **Dashboards** | Opcional; as métricas estão expostas, os painéis não foram construídos |
| **Métrica de DLQ** | As filas de **entrada** e de **eventos** são provisionadas com redrive (DLQ), mas não há um *poller* publicando a profundidade da DLQ como métrica |
| **Cardinalidade de métricas** | As rotas usam o **padrão** (`/wallets/{walletId}`), nunca o valor concreto — decisão deliberada para não explodir séries temporais |
| **Rotação de chaves do Keycloak** | Suportada por re-busca em `kid` desconhecida; o intervalo mínimo entre buscas (30s) é fixo no código, não configurável |

---

## Defeitos que a própria bateria de testes encontrou

Registro honesto: estes são bugs **reais** que existiram neste código e foram
descobertos pelas camadas de teste — não hipóteses.

| # | Defeito | Como apareceu | Correção |
|---|---|---|---|
| 1 | **Ledger gravado antes da transação**, violando `wallet_ledger_entries_transaction_id_fkey` | **Teste de fumaça** ponta a ponta (a suíte de integração montava as inserções à mão, na ordem certa) | Reordenar as escritas: saldo → **transação** → ledger → outbox |
| 2 | **Resumo do relay sempre "reagendado"** — o laço lia `record.PublishedAt` de um registro recebido **por valor**, então as mutações internas não apareciam | Teste unitário sobre o **resumo** da rodada | O método passou a devolver `published bool` explicitamente |
| 3 | **`context.Background()` dentro do caso de uso**, ignorando cancelamento e deadline do pedido | Revisão ao escrever o adapter | `newTransactionEntity` passou a receber o `ctx` da transação |
| 4 | **409 falso sob concorrência.** A verificação do par externo devolvia conflito direto, assumindo que "chave não encontrada + par encontrado ⇒ outra chave foi usada". **Premissa inválida**: entre as duas consultas, a requisição vencedora pode ter comitado — e aquele caso é REPLAY, não conflito | **Bateria de concorrência**: 50 requisições paralelas com a mesma chave produziam intermitentemente `409` (e antes disso, `500`) | Duas mudanças: (a) encontrar o par externo passou a **comparar a chave** antes de concluir conflito; (b) releitura da idempotência **depois** do lock da carteira, tornando o replay determinístico |

### O que o defeito #4 ensina

Ele é o exemplo perfeito de por que a bateria do §13 existe. Três observações:

1. **Passou uma vez e falhou depois.** A primeira execução do teste de concorrência
   passou; a segunda falhou. Um bug intermitente é **pior** que um bug constante,
   porque a tentação é "roda de novo que passa". A resposta correta foi rodar
   **10 vezes seguidas** — e, com isso, transformar um "flaky" em um bug confirmado.
2. **O diagnóstico veio do `-race` e da mensagem, não da intuição.** Presumi que a
   causa era a corrida no índice único; a leitura da resposta (`409 ... already exists
   under idempotency key "provider-a:bet-race-1"`) mostrou que era outra: a **segunda
   consulta** respondendo sozinha pelo conflito.
3. **A correção não foi afrouxar o teste.** O teste estava certo; o código é que
   tinha a premissa errada sobre o que as duas consultas significavam.

### Defeitos nos *próprios testes* (para não dar a impressão de que só o código erra)

| Defeito | Causa |
|---|---|
| Teste de expiração de referência sempre verde | Guardava apenas a **última** rodada (que já não encontra nada) em vez de acumular os resultados |
| Asserção de payload de evento errada | Esperava `"balanceBefore":"100.00"` quando o contrato é um **objeto Money** (`{"amount":...,"currency":...}`) |
| `TRUNCATE ... CASCADE` falhando na limpeza | O `CASCADE` **propagava** ao ledger, e o trigger de append-only recusava (a correção foi truncar todas as tabelas num único comando, sem CASCADE) |
| Import com `davidmoraesrego` (sem o "e") | Erro de digitação, várias vezes — o compilador apontou cada um |

---

## Verificação executada

| Verificação | Comando | Resultado |
|---|---|---|
| Unitários (domínio + casos de uso) | `go test ./...` | **87** testes, 6 pacotes, verdes |
| Detector de races | `go test -race ./...` | verde |
| Integração com PostgreSQL real | `go test -tags=integration ./internal/adapters/outbound/postgres/` | **15** testes verdes |
| Concorrência (HTTP + banco reais) | `go test -tags=integration -race ./internal/adapters/inbound/httpapi/` | **4** testes verdes, sem data race |
| Ponta a ponta (autenticação real) | `scripts/smoke-test.ps1` | **29** verificações, todas aprovadas |
| Estabilidade da concorrência | `go test -tags=integration -race ./internal/adapters/inbound/httpapi/ -count=10` | **10 execuções consecutivas verdes** (após a correção do defeito #4) |
| Análise estática | `go vet -tags=integration ./...` · `gofmt -l .` | limpos |
