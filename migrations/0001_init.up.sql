-- Schema inicial. As invariantes financeiras do §5 são impostas AQUI (não só na
-- aplicação): se o Go tiver um bug, o banco ainda recusa estado financeiro inválido.
--
-- Decisões: dinheiro em BIGINT de centavos (nunca NUMERIC/FLOAT); estados/kinds
-- como TEXT + CHECK (mais fácil de evoluir que ENUM); saldo nunca negativo, cadeia
-- de saldo no ledger (balanceAfter = balanceBefore ± amount), LOSS com amount = 0,
-- unicidade de idempotência e de (provider, externalId), OPENING único por carteira;
-- ledger append-only imposto por TRIGGER (UPDATE/DELETE/TRUNCATE recusados).

-- -----------------------------------------------------------------------------
-- 1) WALLETS — raiz do agregado financeiro (§6.2).
--    (playerId, currency) identifica uma única carteira; saldo em unidades
--    mínimas; version incrementa SOMENTE em mudança de saldo (§6.2).
-- -----------------------------------------------------------------------------
CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL,
    balance_minor BIGINT      NOT NULL DEFAULT 0,
    version       BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- §5.7: o banco é a última linha de defesa contra saldo negativo.
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    -- §6.2: a versão começa em 1 e nunca é menor que isso.
    CONSTRAINT wallets_version_positive     CHECK (version >= 1),
    -- Moeda ISO 4217 (3 letras maiúsculas). Evita "brl", "R$", vazio etc.
    CONSTRAINT wallets_currency_iso4217     CHECK (currency ~ '^[A-Z]{3}$'),
    -- §6.2: o par (playerId, currency) é único. Tentar abrir carteira duplicada
    -- vira violação de constraint -> conflito HTTP (§9).
    CONSTRAINT wallets_player_currency_uniq UNIQUE (player_id, currency)
);

-- -----------------------------------------------------------------------------
-- 2) WAGER_TRANSACTIONS — operações internas (OPENING) e externas (§6.3).
--    Guarda idempotência, referência resolvida, estado, código de falha e o
--    resultado financeiro devolvido ao provedor (para replay sem reaplicar).
-- -----------------------------------------------------------------------------
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL,   -- INTERNAL | EXTERNAL
    kind                              TEXT        NOT NULL,   -- OPENING|BET|WIN|LOSS|REFUND|ROLLBACK
    state                             TEXT        NOT NULL,   -- PENDING|PENDING_REFERENCE|PROCESSED|REJECTED|FAILED
    player_id                         UUID        NOT NULL,
    wallet_id                         UUID        NOT NULL REFERENCES wallets(id),
    currency                          CHAR(3)     NOT NULL,
    amount_minor                      BIGINT      NOT NULL,

    -- Metadados externos (§6.3). Nulos para OPENING.
    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      TEXT,
    round_id                          TEXT,
    game_id                           TEXT,

    -- Referência de reversão: externa (obrigatória p/ REFUND/ROLLBACK) e a
    -- interna resolvida (quando aplicável, §6.3).
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID        REFERENCES wager_transactions(id),

    -- Correlação de rastreio (§12). Persistida porque a operação RETOMADA pelo
    -- worker de referências precisa conservar o `correlationId` do pedido
    -- ORIGINAL: sem isso, o rastro se romperia exatamente no ponto em que mais
    -- precisamos dele (uma operação que ficou pendente e foi resolvida depois).
    correlation_id                    TEXT,

    -- Resultado: código de falha estável + saldo resultante observado (§9).
    failure_code                      TEXT,
    result_balance_minor              BIGINT,

    -- Retomada durável de PENDING_REFERENCE (§7): backoff exponencial via
    -- attempts + next_retry_at.
    attempts                          INTEGER     NOT NULL DEFAULT 0,
    next_retry_at                     TIMESTAMPTZ,

    created_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                        TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at                      TIMESTAMPTZ,

    CONSTRAINT wtx_origin_valid   CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    CONSTRAINT wtx_kind_valid     CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wtx_state_valid    CHECK (state IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    CONSTRAINT wtx_currency_valid CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wtx_amount_non_negative CHECK (amount_minor >= 0),

    -- §7: LOSS exige "0.00"; BET/WIN/REFUND/ROLLBACK exigem > 0; OPENING aceita
    -- saldo inicial zero (que NÃO cria OPENING) ou positivo.
    CONSTRAINT wtx_amount_by_kind CHECK (
        (kind = 'LOSS'    AND amount_minor = 0)
        OR (kind = 'OPENING' AND amount_minor >= 0)
        OR (kind NOT IN ('LOSS', 'OPENING') AND amount_minor > 0)
    ),

    -- §6.3: OPENING é estritamente interno; recusar OPENING vindo de HTTP/SQS
    -- é reforçado aqui (o domínio também valida).
    CONSTRAINT wtx_opening_is_internal CHECK (kind <> 'OPENING' OR origin = 'INTERNAL'),

    -- §6.3: o schema distingue origem. Internas NÃO carregam metadados externos;
    -- externas exigem todos eles.
    CONSTRAINT wtx_origin_fields CHECK (
        (origin = 'INTERNAL' AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL)
        OR
        (origin = 'EXTERNAL' AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    )
);

-- §9/§5.2: idempotência persistente. Índice único PARCIAL porque OPENING
-- (interno) não tem chave. Único global, não por provedor: a chave é a
-- identidade da operação para o cliente.
CREATE UNIQUE INDEX wtx_idempotency_key_uniq
    ON wager_transactions (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- §9: (providerId, externalTransactionId) identifica uma operação financeira.
-- Impede reaplicar a mesma operação usando OUTRA chave de idempotência.
CREATE UNIQUE INDEX wtx_provider_external_uniq
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE origin = 'EXTERNAL';

-- §6.3: impede crédito inicial duplicado — no máximo um OPENING por carteira.
CREATE UNIQUE INDEX wtx_opening_per_wallet_uniq
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';

-- §7: no máximo UMA reversão bem-sucedida por referência. Como o índice é
-- parcial em state='PROCESSED', ele passa a valer no instante em que uma
-- reversão é confirmada — impossibilitando, no nível do banco, que a mesma
-- referência receba duas devoluções (ex.: REFUND e depois ROLLBACK da BET).
CREATE UNIQUE INDEX wtx_reversal_once_per_reference_uniq
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE kind IN ('REFUND', 'ROLLBACK') AND state = 'PROCESSED';

-- Índice para o worker de referências pendentes varrer o que já venceu.
CREATE INDEX wtx_pending_reference_due_idx
    ON wager_transactions (next_retry_at)
    WHERE state = 'PENDING_REFERENCE';

-- Índice para consulta/histórico por carteira.
CREATE INDEX wtx_wallet_created_idx
    ON wager_transactions (wallet_id, created_at);

-- -----------------------------------------------------------------------------
-- 3) WALLET_LEDGER_ENTRIES — ledger append-only (§6.4).
--    Cada lançamento carrega o saldo antes e depois; a construção valida
--    balanceAfter = balanceBefore ± money conforme a direção.
--    LOSS e operações rejeitadas NÃO produzem lançamentos (§6.4) — por isso a
--    unicidade é (wallet_id, transaction_id): no máximo um lançamento por tx.
-- -----------------------------------------------------------------------------
CREATE TABLE wallet_ledger_entries (
    id                   BIGSERIAL   PRIMARY KEY,  -- ordem estável p/ cursor de paginação (§9)
    wallet_id            UUID        NOT NULL REFERENCES wallets(id),
    transaction_id       UUID        NOT NULL REFERENCES wager_transactions(id),
    direction            TEXT        NOT NULL,     -- DEBIT | CREDIT
    amount_minor         BIGINT      NOT NULL,
    balance_before_minor BIGINT      NOT NULL,
    balance_after_minor  BIGINT      NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_direction_valid CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ledger_balance_non_negative CHECK (balance_after_minor >= 0),

    -- §6.4: a cadeia de saldo é uma invariante do banco, não da aplicação.
    CONSTRAINT ledger_balance_chain CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR
        (direction = 'DEBIT'  AND balance_after_minor = balance_before_minor - amount_minor)
    ),

    -- §6.4: unicidade (walletId, transactionId) imposta pelo schema.
    CONSTRAINT ledger_wallet_transaction_uniq UNIQUE (wallet_id, transaction_id)
);

-- Paginação por cursor do extrato (§9): consultas por (wallet_id, id).
CREATE INDEX ledger_wallet_id_idx ON wallet_ledger_entries (wallet_id, id);

-- §5.5/§6.4: ledger append-only. A função abaixo é reaproveitada por UPDATE,
-- DELETE e TRUNCATE — qualquer mutação levanta exceção e aborta a transação.
CREATE OR REPLACE FUNCTION ledger_forbid_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only: % is not allowed', TG_OP
        USING ERRCODE = '55000'; -- object_not_in_prerequisite_state
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_no_update
    BEFORE UPDATE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();

CREATE TRIGGER ledger_no_delete
    BEFORE DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_forbid_mutation();

-- TRUNCATE exige trigger no nível de STATEMENT (não FOR EACH ROW).
CREATE TRIGGER ledger_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_forbid_mutation();

-- Defesa em profundidade: além do trigger, revogamos o privilégio. O trigger é
-- a garantia definitiva porque o owner da tabela pode contornar REVOKE.
REVOKE UPDATE, DELETE, TRUNCATE ON wallet_ledger_entries FROM PUBLIC;

-- -----------------------------------------------------------------------------
-- 4) INBOX_MESSAGES — deduplicação do consumidor SQS (§6.5/§10).
--    Unicidade (consumerName, messageId). Guardamos o hash do corpo para
--    detectar reentrega com conteúdo divergente para o MESMO messageId (§10).
-- -----------------------------------------------------------------------------
CREATE TABLE inbox_messages (
    consumer_name TEXT        NOT NULL,
    message_id    TEXT        NOT NULL,
    message_hash  TEXT        NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ,

    CONSTRAINT inbox_consumer_message_pk PRIMARY KEY (consumer_name, message_id)
);

-- -----------------------------------------------------------------------------
-- 5) OUTBOX_EVENTS — Transactional Outbox (§6.5/§11).
--    O publisher só publica DEPOIS do commit (§5.4). eventId é estável e é
--    reusado em republicações (§11). next_attempt_at implementa backoff.
-- -----------------------------------------------------------------------------
CREATE TABLE outbox_events (
    id              UUID        PRIMARY KEY,           -- eventId estável (§11)
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    event_version   INTEGER     NOT NULL DEFAULT 1,
    payload         JSONB       NOT NULL,               -- snapshot imutável (§11)
    correlation_id  TEXT,
    causation_id    TEXT,
    occurred_at     TIMESTAMPTZ NOT NULL,
    attempts        INTEGER     NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT outbox_event_type_valid CHECK (
        event_type IN (
            'WagerTransactionProcessed',
            'WagerTransactionRejected',
            'WalletBalanceChanged',
            'WagerTransactionPendingReference'
        )
    )
);

-- Índice do polling do publisher: só linhas pendentes, ordenadas pelo próximo
-- horário de tentativa. É o índice que torna `FOR UPDATE SKIP LOCKED` eficiente
-- e permite MÚLTIPLOS publishers disputarem sem locker global (§11).
CREATE INDEX outbox_pending_idx
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL;
