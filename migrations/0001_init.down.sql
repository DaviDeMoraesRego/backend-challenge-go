-- =============================================================================
-- migrations/0001_init.down.sql
-- =============================================================================
-- PROPÓSITO
--   Reverter EXATAMENTE o que 0001_init.up.sql criou, na ordem inversa de
--   dependência. Atende ao §4/§15 ("reversão documentada").
--
-- CAMADA: infrastructure bootstrap (schema).
--
-- ORDEM DE DROP (por que importa)
--   1. Tabelas com FK para outras tabelas caem primeiro (ledger -> transactions
--      -> wallets), senão o DROP falha por dependência.
--   2. Triggers e a função do ledger caem junto com a tabela; ainda assim,
--      removemos a função explicitamente porque ela é um objeto compartilhado.
--   3. Índices parciais criados fora das tabelas são removidos pelo DROP TABLE
--      (Postgres remove índices da tabela automaticamente).
--
-- ARMADILHA CLÁSSICA
--   Rodar o down sem DROP ORDER correto resulta em erro
--   "cannot drop table wallets because other objects depend on it". A ordem
--   abaixo é o inverso exato de criação.
-- =============================================================================

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS inbox_messages;

-- Triggers caem com a tabela; DROP FUNCTION exige remover a dependência antes.
DROP TRIGGER IF EXISTS ledger_no_truncate ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS ledger_no_delete ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS ledger_no_update ON wallet_ledger_entries;
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP FUNCTION IF EXISTS ledger_forbid_mutation();

DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;
