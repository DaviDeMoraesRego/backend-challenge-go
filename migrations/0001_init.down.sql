-- Reverte 0001 na ordem inversa de dependência (ledger -> transactions -> wallets),
-- senão o DROP falha com "cannot drop table wallets because other objects depend
-- on it". A função do ledger é removida explicitamente (objeto compartilhado).

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
