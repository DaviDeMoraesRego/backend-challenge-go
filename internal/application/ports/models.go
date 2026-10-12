package ports

import (
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// Scope é o mundo de acesso derivado da identidade autenticada (§2). Os valores
// casam com a claim `service_scope` do Keycloak (deploy/keycloak/realm-export.json).
type Scope string

const (
	// ScopeInternal: serviço interno. Acessa abertura de carteira e reconciliação.
	ScopeInternal Scope = "internal"

	// ScopeProvider: provedor de jogos. Acessa apenas as PRÓPRIAS transações.
	ScopeProvider Scope = "provider"
)

// Principal é a identidade autenticada. O `ProviderID` autorizado vem daqui, nunca
// do corpo da requisição — é o que impede o provedor A de agir como B, inclusive em
// replays (§2).
type Principal struct {
	Scope      Scope
	ProviderID string
}

func (p Principal) IsInternal() bool { return p.Scope == ScopeInternal }

func (p Principal) IsProvider() bool { return p.Scope == ScopeProvider }

// OpenWalletCommand cria uma carteira. Campos em texto: a conversão para money.Money
// acontece no caso de uso.
type OpenWalletCommand struct {
	// Principal: a abertura é restrita ao serviço interno; senão ErrForbidden (§2).
	Principal Principal

	PlayerID        string
	InitialAmount   string // ex.: "1000.00"
	InitialCurrency string // ex.: "BRL"
	CorrelationID   string
}

// OpenWalletResult devolve a carteira recém-criada (§9).
type OpenWalletResult struct {
	WalletID wallet.ID
	PlayerID wallet.PlayerID
	Balance  money.Money
	Version  int64
}

// PlaceOperationCommand é a entrada única de operações, usada por HTTP e SQS. A chave
// de idempotência é explícita (HTTP: header `Idempotency-Key`; SQS: data.idempotencyKey,
// §10) — o servidor não a substitui por uma calculada (§9).
type PlaceOperationCommand struct {
	Principal Principal

	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       string
	WalletID                       string
	RoundID                        string
	GameID                         string
	Kind                           string
	Amount                         string
	Currency                       string
	ReferenceExternalTransactionID string

	CorrelationID string
	CausationID   string

	// Inbox: preenchido apenas na entrada por SQS (§6.5/§10).
	InboxConsumer    string
	InboxMessageID   string
	InboxMessageHash string
}

// PlaceOperationResult é o resultado devolvido ao provedor (§9).
type PlaceOperationResult struct {
	TransactionID transaction.ID
	State         transaction.State

	// FailureCode só é preenchido quando o estado é REJECTED ou FAILED.
	FailureCode transaction.FailureCode

	// Balance é o saldo observado no processamento original — em replay, devolvido
	// igual, mesmo que a carteira já tenha mudado depois (§9).
	Balance money.Money

	// BalanceKnown é falso nas rejeições de negócio e em PENDING_REFERENCE (não houve
	// resultado a observar); nesse caso o adapter OMITE `balance` em vez de publicar um
	// "0.00" que o provedor leria como saldo real.
	BalanceKnown bool

	// IdempotentReplay: esta chamada não aplicou nada novo, só devolveu o resultado
	// persistido (§9).
	IdempotentReplay bool
}

// GetWalletQuery busca uma carteira por ID.
type GetWalletQuery struct {
	Principal Principal
	WalletID  string
}

// WalletView é a projeção devolvida ao cliente (§9).
type WalletView struct {
	WalletID wallet.ID
	PlayerID wallet.PlayerID
	Balance  money.Money
	Version  int64
}

// GetLedgerQuery pede uma página do extrato. `Cursor` é o `id` do último lançamento
// recebido (opaco para o cliente, §9).
type GetLedgerQuery struct {
	Principal Principal
	WalletID  string
	Cursor    int64 // 0 = primeira página
	Limit     int
}

// LedgerEntryView é um lançamento projetado para o cliente (§6.4/§9).
type LedgerEntryView struct {
	ID            int64
	TransactionID transaction.ID
	Direction     wallet.Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	CreatedAt     time.Time
}

// LedgerPage é uma página do extrato com o cursor da próxima página.
type LedgerPage struct {
	Entries    []LedgerEntryView
	NextCursor int64 // 0 = não há mais páginas
}

// GetTransactionQuery busca uma transação pelo ID interno (§9).
type GetTransactionQuery struct {
	Principal     Principal
	TransactionID string
}

// GetTransactionByExternalIDQuery busca pelo par (providerId, externalTransactionId) (§9).
type GetTransactionByExternalIDQuery struct {
	Principal Principal

	// ProviderID vem da rota e é validado contra a identidade: um provedor só consulta
	// o próprio. O serviço interno pode informar qualquer um (§2).
	ProviderID            string
	ExternalTransactionID string
}

// TransactionView é a projeção de uma transação para consulta (§9). Inclui
// FailureCode e Correctable para o cliente acompanhar pendências e rejeições.
type TransactionView struct {
	TransactionID         transaction.ID
	ProviderID            transaction.ProviderID
	ExternalTransactionID transaction.ExternalTransactionID
	PlayerID              wallet.PlayerID
	WalletID              wallet.ID
	RoundID               string
	GameID                string
	Kind                  transaction.Kind
	State                 transaction.State
	Amount                money.Money
	ReferenceExternalID   transaction.ExternalTransactionID
	FailureCode           transaction.FailureCode
	Correctable           bool
	ResultBalance         money.Money
	HasResultBalance      bool
	Attempts              int
	NextRetryAt           time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	ProcessedAt           time.Time
}

// ReconcileWalletQuery pede a reconciliação de uma carteira.
type ReconcileWalletQuery struct {
	Principal Principal
	WalletID  string
}

// ReconcileWalletResult é o relatório de reconciliação (§9). `Difference` é
// storedBalance - calculatedBalance; a reconciliação não altera o saldo.
type ReconcileWalletResult struct {
	WalletID          wallet.ID
	StoredBalance     money.Money
	CalculatedBalance money.Money
	Difference        money.Money
	Consistent        bool
	CheckedEntries    int
}

// ResolvePendingReferencesResult resume uma rodada de resolução de pendências.
type ResolvePendingReferencesResult struct {
	Scanned     int
	Resolved    int
	Rescheduled int
	Rejected    int
	Failed      int
}

// RelayOutboxResult resume uma rodada do relay do outbox (§11).
type RelayOutboxResult struct {
	Claimed     int // reservados na rodada
	Published   int // entregues ao broker com sucesso
	Rescheduled int // falharam e foram reagendados com backoff
	Failed      int // esgotaram as tentativas e foram desistidos

	// MaxLag é o maior atraso da rodada (ocorrência -> publicação); alimenta
	// wager_outbox_lag_seconds.
	MaxLag time.Duration
}

// OutboxRecord é o registro de evento pronto para publicação. O payload vai já
// serializado (JSON snapshot imutável, §11): o conteúdo publicado é exatamente o
// gravado no commit, sem re-renderizar em republicações tardias.
type OutboxRecord struct {
	EventID       string
	AggregateType string
	AggregateID   string
	EventType     string
	EventVersion  int
	Payload       []byte
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
	Attempts      int
	NextAttemptAt time.Time
	PublishedAt   time.Time
}
