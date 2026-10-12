-- Reverte 0002: restaura o índice parcial anterior e remove a coluna.

DROP INDEX IF EXISTS outbox_pending_idx;
CREATE INDEX outbox_pending_idx
    ON outbox_events (next_attempt_at, created_at)
    WHERE published_at IS NULL;

ALTER TABLE outbox_events DROP COLUMN IF EXISTS dead_at;
