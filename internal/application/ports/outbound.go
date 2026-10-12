package ports

import (
	"context"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// Os adapters outbound traduzem erros nativos (constraint, deadlock, timeout) para
// os sentinelas de errors.go; os casos de uso classificam por errors.Is (§6).

// Clock abstrai o relógio para testes determinísticos (backoff e TTL dependem do
// instante) e para o caso de uso usar o mesmo `now` na carteira, ledger e eventos.
type Clock interface {
	Now() time.Time
}

// IDGenerator abstrai a geração de identificadores (UUIDv7 no adapter), mantendo o
// domínio sem dependências externas.
type IDGenerator interface {
	// NewID devolve um identificador textual novo e estável.
	NewID(ctx context.Context) (string, error)
}

// WalletRepository é o acesso ao agregado Wallet.
type WalletRepository interface {
	// Insert cria a carteira e sua versão inicial.
	Insert(ctx context.Context, w *wallet.Wallet) error

	// FindByPlayerAndCurrency localiza a carteira pelo par natural (§6.2).
	// Devolve ErrNotFound se não existir.
	FindByPlayerAndCurrency(ctx context.Context, playerID wallet.PlayerID, currency money.Currency) (*wallet.Wallet, error)

	// FindByID localiza a carteira por identidade, sem lock.
	FindByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error)

	// LockByID aplica `SELECT ... FOR UPDATE` na linha: estratégia de concorrência por
	// carteira do §8 (nunca global, §5.6), serializando escritas entre processos.
	LockByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error)

	// Update persiste saldo, versão e `updatedAt`.
	Update(ctx context.Context, w *wallet.Wallet) error
}

// WagerTransactionRepository é o acesso ao agregado Transaction.
type WagerTransactionRepository interface {
	// Insert grava a transação no estado atual; violação de unicidade vira ErrAlreadyExists.
	Insert(ctx context.Context, tx *transaction.Transaction) error

	// Update persiste transições, referência resolvida, tentativas e resultado.
	Update(ctx context.Context, tx *transaction.Transaction) error

	// FindByIDempotencyKey é a primeira consulta do fluxo de escrita (§9).
	FindByIDempotencyKey(ctx context.Context, key transaction.IdempotencyKey) (*transaction.Transaction, error)

	// FindByProviderAndExternalID suporta a consulta por par externo e a detecção de
	// reuso indevido de (providerId, externalTransactionId) (§9).
	FindByProviderAndExternalID(ctx context.Context, providerID transaction.ProviderID, externalID transaction.ExternalTransactionID) (*transaction.Transaction, error)

	// FindByID busca pela identidade interna.
	FindByID(ctx context.Context, id transaction.ID) (*transaction.Transaction, error)

	// HasSuccessfulReversal informa se a referência já tem reversão confirmada (§7).
	HasSuccessfulReversal(ctx context.Context, providerID transaction.ProviderID, referenceExternalID transaction.ExternalTransactionID) (bool, error)

	// ListDuePendingReferences devolve PENDING_REFERENCE vencidas, com `now` do Clock e
	// lote limitado (§7).
	ListDuePendingReferences(ctx context.Context, now time.Time, limit int) ([]*transaction.Transaction, error)
}

// LedgerRepository é o acesso ao ledger append-only (§6.4/§5.5).
type LedgerRepository interface {
	// Append insere um lançamento; não há Update/Delete (trigger recusa mutação no banco).
	Append(ctx context.Context, entry *ledger.Entry) error

	// ListByWallet devolve página ordenada por `id` crescente a partir do cursor (§9).
	ListByWallet(ctx context.Context, walletID wallet.ID, afterID int64, limit int) ([]*ledger.Entry, error)

	// SumSignedAmounts reconstrói o saldo somando os lançamentos com sinal contábil (§9).
	SumSignedAmounts(ctx context.Context, walletID wallet.ID) (money.Money, error)

	// CountByWallet devolve `checkedEntries` da reconciliação (§9).
	CountByWallet(ctx context.Context, walletID wallet.ID) (int, error)
}

// InboxMessage é o registro de uma mensagem recebida (modelo de leitura).
type InboxMessage struct {
	ConsumerName string
	MessageID    string
	MessageHash  string
	ReceivedAt   time.Time
	ProcessedAt  time.Time
}

// InboxRepository deduplica mensagens do consumidor (§6.5/§10).
type InboxRepository interface {
	// Register insere a mensagem; ErrAlreadyExists indica REENTREGA (não reaplicar efeitos).
	Register(ctx context.Context, consumerName, messageID, messageHash string) error

	// MarkProcessed marca a conclusão durável, na mesma transação do domínio (§6.5).
	MarkProcessed(ctx context.Context, consumerName, messageID string) error

	// Find recupera o registro para comparar o hash em reentregas (§10).
	Find(ctx context.Context, consumerName, messageID string) (*InboxMessage, error)
}

// OutboxRepository grava eventos para publicação posterior (§6.5/§11).
type OutboxRepository interface {
	// Append grava eventos na mesma transação do domínio (garante o §5.4).
	Append(ctx context.Context, records ...OutboxRecord) error

	// ClaimPending reserva um lote pendente (FOR UPDATE SKIP LOCKED, §11), permitindo
	// múltiplos publishers sem locker global. `leaseUntil` (política do caso de uso)
	// vira o novo next_attempt_at: o lock é liberado já e a recuperação de trabalho
	// abandonado acontece por vencimento do lease.
	ClaimPending(ctx context.Context, now time.Time, leaseUntil time.Time, limit int) ([]OutboxRecord, error)

	// MarkPublished confirma a publicação e libera a reserva.
	MarkPublished(ctx context.Context, eventID string, publishedAt time.Time) error

	// Reschedule reagenda um evento falho com backoff exponencial (§11).
	Reschedule(ctx context.Context, eventID string, attempts int, nextAttemptAt time.Time) error

	// MarkDead marca o evento como desistido após esgotar as tentativas: permanece
	// auditável, mas sai do polling. Sem isso o relay o republicaria para sempre.
	MarkDead(ctx context.Context, eventID string, deadAt time.Time) error
}

// TxScope expõe os repositórios dentro de uma transação. O caso de uso nunca vê
// `pgx.Tx`: vê repositórios que compartilham a mesma transação SQL — é o que garante
// atomicidade entre saldo, ledger, inbox e outbox sem acoplar ao driver.
type TxScope interface {
	Wallets() WalletRepository
	Transactions() WagerTransactionRepository
	Ledger() LedgerRepository
	Inbox() InboxRepository
	Outbox() OutboxRepository
}

// UnitOfWork executa um bloco em uma transação. Contrato: nil -> COMMIT; erro ->
// ROLLBACK; panic -> ROLLBACK antes de propagar (esquecer o rollback deixa a conexão
// pendurada no pool até o timeout do servidor).
type UnitOfWork interface {
	Execute(ctx context.Context, fn func(ctx context.Context, scope TxScope) error) error
}

// EventPublisher entrega um registro de outbox ao broker. `groupID` (MessageGroupId da
// fila FIFO) é parâmetro explícito: a ordenação é decidida pela camada que conhece a
// fila, mas o valor (id do agregado) é semântico.
type EventPublisher interface {
	Publish(ctx context.Context, record OutboxRecord, groupID string) error
}
