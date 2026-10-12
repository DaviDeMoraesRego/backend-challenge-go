package ports

import "context"

// OpenWallet abre uma carteira (§9: POST /wallets). Com saldo inicial positivo cria
// a transação OPENING, o lançamento de crédito e os eventos WagerTransactionProcessed
// e WalletBalanceChanged no mesmo commit da carteira; saldo zero não cria nada disso.
type OpenWallet interface {
	Execute(ctx context.Context, cmd OpenWalletCommand) (OpenWalletResult, error)
}

// PlaceWagerOperation é o caso de uso compartilhado por HTTP e SQS (§10).
//
// Ordem: autorização (providerId vem de cmd.Principal, §2); parsing estrito da
// entrada (único ponto de conversão nos dois canais); idempotência (mesmo hash ->
// replay, hash diferente -> conflito, §9); regras por tipo (§7); efeitos (wallet,
// ledger, outbox na mesma transação, §5.3/§5.4); inbox na mesma transação quando o
// comando vem do SQS (§6.5).
//
// Rejeição de negócio NÃO é erro: volta em PlaceOperationResult.FailureCode com
// State == REJECTED (§6.3/§7).
type PlaceWagerOperation interface {
	Execute(ctx context.Context, cmd PlaceOperationCommand) (PlaceOperationResult, error)
}

// GetWallet consulta uma carteira (§9: GET /wallets/:walletId).
type GetWallet interface {
	Execute(ctx context.Context, query GetWalletQuery) (WalletView, error)
}

// GetWalletLedger consulta o extrato paginado (§9:
// GET /wallets/:walletId/ledger?cursor=...&limit=50).
type GetWalletLedger interface {
	Execute(ctx context.Context, query GetLedgerQuery) (LedgerPage, error)
}

// GetWagerTransaction consulta uma transação interna (§9:
// GET /wagering/transactions/:transactionId).
type GetWagerTransaction interface {
	Execute(ctx context.Context, query GetTransactionQuery) (TransactionView, error)
}

// GetWagerTransactionByExternalID consulta pelo par externo (§9:
// GET /providers/:providerId/wagering/transactions/:externalTransactionId).
type GetWagerTransactionByExternalID interface {
	Execute(ctx context.Context, query GetTransactionByExternalIDQuery) (TransactionView, error)
}

// ReconcileWallet confronta saldo armazenado com o reconstruído do ledger (§9).
// É read-only: divergências vão para resposta, logs e métrica, sem corrigir o saldo.
type ReconcileWallet interface {
	Execute(ctx context.Context, query ReconcileWalletQuery) (ReconcileWalletResult, error)
}

// ResolvePendingReferences é a rodada do worker de referências (§7): resolve a
// referência vencida, reagenda com backoff ou finaliza como REJECTED no fim das
// tentativas/TTL.
type ResolvePendingReferences interface {
	Execute(ctx context.Context) (ResolvePendingReferencesResult, error)
}

// RelayOutbox é a rodada do publicador do outbox (§11): reserva um lote com
// FOR UPDATE SKIP LOCKED, publica e confirma; falhas são reagendadas com backoff e a
// republicação preserva o `eventId`.
type RelayOutbox interface {
	Execute(ctx context.Context) (RelayOutboxResult, error)
}
