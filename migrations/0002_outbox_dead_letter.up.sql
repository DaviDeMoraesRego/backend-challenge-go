-- Dead-letter do outbox: um evento que esgota as tentativas era só reagendado e o
-- relay o republicava para sempre. `dead_at` o mantém auditável mas fora do polling.

ALTER TABLE outbox_events ADD COLUMN dead_at TIMESTAMPTZ;

-- O índice de polling cobre só linhas publicáveis (nem publicadas, nem mortas).
DROP INDEX IF EXISTS outbox_pending_idx;
CREATE INDEX outbox_pending_idx
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL AND dead_at IS NULL;
