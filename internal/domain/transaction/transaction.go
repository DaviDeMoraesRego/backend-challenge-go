package transaction

import (
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var (
	ErrUninitialized = fmt.Errorf("transaction: uninitialized value")

	// ErrMissingReference: REFUND/ROLLBACK sem referência (§7).
	ErrMissingReference = fmt.Errorf("transaction: reversal requires a reference")

	// ErrUnexpectedReference: tipo não-reversão recebeu referência.
	ErrUnexpectedReference = fmt.Errorf("transaction: non-reversal must not carry a reference")

	// ErrInvalidAmount: valor em desacordo com a regra do tipo (§7).
	ErrInvalidAmount = fmt.Errorf("transaction: invalid amount for kind")

	// ErrLossAmountNotZero: LOSS exige valor exatamente "0.00" (§7).
	ErrLossAmountNotZero = fmt.Errorf("transaction: LOSS requires amount 0.00")

	// ErrOpeningRequiresPositiveAmount: abertura com saldo zero não gera OPENING (§9).
	ErrOpeningRequiresPositiveAmount = fmt.Errorf("transaction: OPENING requires a positive amount")

	// ErrOpeningNotAllowedExternally: OPENING enviado por HTTP/SQS (§6.3).
	ErrOpeningNotAllowedExternally = fmt.Errorf("transaction: OPENING is internal only")

	ErrMissingExternalMetadata = fmt.Errorf("transaction: external operation is missing required metadata")
	ErrInvalidTimestamp        = fmt.Errorf("transaction: invalid timestamp")
	ErrInvalidFailureCode      = fmt.Errorf("transaction: invalid failure code")
	ErrNegativeAttempts        = fmt.Errorf("transaction: negative attempts")

	// ErrInconsistentOrigin: origem incoerente com o tipo ou metadados (§6.3).
	ErrInconsistentOrigin = fmt.Errorf("transaction: inconsistent origin")
)

// OptionalMoney representa um Money que pode estar ausente (padrão sql.Null):
// valor + flag, em vez de ponteiro ambíguo entre nil e zero.
type OptionalMoney struct {
	Value money.Money
	Valid bool
}

func SomeMoney(m money.Money) OptionalMoney { return OptionalMoney{Value: m, Valid: true} }

func NoMoney() OptionalMoney { return OptionalMoney{} }

// Transaction é o agregado da operação de aposta (§6.3).
type Transaction struct {
	id     ID
	origin Origin
	kind   Kind
	state  State

	playerID wallet.PlayerID
	walletID wallet.ID
	amount   money.Money

	// Metadados externos (§6.3). Vazios quando origin == INTERNAL.
	providerID            ProviderID
	externalTransactionID ExternalTransactionID
	idempotencyKey        IdempotencyKey
	payloadHash           PayloadHash
	roundID               string
	gameID                string

	// Ligado ao pedido original (§12), não ao provedor: gerado no edge ou propagado
	// do SQS, precisa atravessar operações retomadas.
	correlationID string

	// Referência de reversão (§7).
	referenceExternalTransactionID ExternalTransactionID
	referenceTransactionID         OptionalID

	failureCode   FailureCode
	resultBalance OptionalMoney // saldo observado no processamento (§9)

	// Retomada durável de PENDING_REFERENCE (§7).
	attempts    int
	nextRetryAt time.Time // zero = sem agendamento

	createdAt   time.Time
	updatedAt   time.Time
	processedAt time.Time // zero = ainda não processada
}

// OptionalID representa a referência interna resolvida (pode não existir ainda).
type OptionalID struct {
	Value ID
	Valid bool
}

func SomeID(id ID) OptionalID { return OptionalID{Value: id, Valid: true} }

func NoID() OptionalID { return OptionalID{} }

// -----------------------------------------------------------------------------
// Construção
// -----------------------------------------------------------------------------

// NewExternalParams reúne os campos de uma operação de PROVEDOR (HTTP/SQS).
type NewExternalParams struct {
	ID                             ID
	Kind                           Kind
	PlayerID                       wallet.PlayerID
	WalletID                       wallet.ID
	Amount                         money.Money
	ProviderID                     ProviderID
	ExternalTransactionID          ExternalTransactionID
	IdempotencyKey                 IdempotencyKey
	PayloadHash                    PayloadHash
	RoundID                        string
	GameID                         string
	CorrelationID                  string
	ReferenceExternalTransactionID ExternalTransactionID // obrigatório p/ REFUND/ROLLBACK
	Now                            time.Time
}

// NewExternal cria uma operação externa em PENDING. As regras do §7 dependentes do
// tipo vivem aqui: valor zero só em LOSS, positivo nos demais, e referência
// obrigatória em reversões e proibida nos demais tipos.
func NewExternal(p NewExternalParams) (*Transaction, error) {
	// Tipo precisa existir e não pode ser OPENING (§6.3).
	if !p.Kind.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, p.Kind)
	}
	if p.Kind.IsInternal() {
		return nil, fmt.Errorf("%w: kind=%s", ErrOpeningNotAllowedExternally, p.Kind)
	}

	if err := validateID(p.ID); err != nil {
		return nil, err
	}
	if isBlank(string(p.PlayerID)) {
		return nil, fmt.Errorf("%w: player id", ErrInvalidID)
	}
	if isBlank(string(p.WalletID)) {
		return nil, fmt.Errorf("%w: wallet id", ErrInvalidID)
	}

	// Metadados externos obrigatórios (§6.3).
	if isBlank(string(p.ProviderID)) {
		return nil, fmt.Errorf("%w: providerId", ErrMissingExternalMetadata)
	}
	if isBlank(string(p.ExternalTransactionID)) {
		return nil, fmt.Errorf("%w: externalTransactionId", ErrMissingExternalMetadata)
	}
	if isBlank(string(p.IdempotencyKey)) {
		return nil, fmt.Errorf("%w: idempotencyKey", ErrMissingExternalMetadata)
	}
	if isBlank(string(p.PayloadHash)) {
		return nil, fmt.Errorf("%w: payloadHash", ErrMissingExternalMetadata)
	}
	if isBlank(p.RoundID) {
		return nil, fmt.Errorf("%w: roundId", ErrMissingExternalMetadata)
	}
	if isBlank(p.GameID) {
		return nil, fmt.Errorf("%w: gameId", ErrMissingExternalMetadata)
	}

	if p.Now.IsZero() {
		return nil, fmt.Errorf("%w: creation time", ErrInvalidTimestamp)
	}
	if p.Amount.Currency().IsZero() {
		return nil, fmt.Errorf("%w: amount has no currency", money.ErrUninitialized)
	}
	if err := validateAmountForKind(p.Kind, p.Amount); err != nil {
		return nil, err
	}

	// Referência obrigatória em reversões, proibida nos demais tipos.
	if p.Kind.IsReversal() {
		if isBlank(string(p.ReferenceExternalTransactionID)) {
			return nil, fmt.Errorf("%w: kind=%s", ErrMissingReference, p.Kind)
		}
	} else if !isBlank(string(p.ReferenceExternalTransactionID)) {
		return nil, fmt.Errorf("%w: kind=%s", ErrUnexpectedReference, p.Kind)
	}

	now := p.Now.UTC()
	return &Transaction{
		id:                             p.ID,
		origin:                         OriginExternal,
		kind:                           p.Kind,
		state:                          StatePending, // §6.3: inicia em PENDING
		playerID:                       p.PlayerID,
		walletID:                       p.WalletID,
		amount:                         p.Amount,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		correlationID:                  p.CorrelationID,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		referenceTransactionID:         NoID(),
		resultBalance:                  NoMoney(),
		attempts:                       0,
		createdAt:                      now,
		updatedAt:                      now,
	}, nil
}

// NewOpeningParams reúne os campos da ABERTURA INTERNA de carteira (§6.3/§9).
type NewOpeningParams struct {
	ID       ID
	PlayerID wallet.PlayerID
	WalletID wallet.ID
	Amount   money.Money
	Now      time.Time
}

// NewOpening cria a transação de abertura já em PROCESSED: a abertura é atômica
// com a criação da carteira, sem etapa assíncrona (§9). Exige valor positivo —
// saldo inicial zero não gera OPENING, então o chamador só invoca aqui nesse caso.
func NewOpening(p NewOpeningParams) (*Transaction, error) {
	if err := validateID(p.ID); err != nil {
		return nil, err
	}
	if isBlank(string(p.PlayerID)) {
		return nil, fmt.Errorf("%w: player id", ErrInvalidID)
	}
	if isBlank(string(p.WalletID)) {
		return nil, fmt.Errorf("%w: wallet id", ErrInvalidID)
	}
	if p.Now.IsZero() {
		return nil, fmt.Errorf("%w: creation time", ErrInvalidTimestamp)
	}
	if p.Amount.Currency().IsZero() {
		return nil, fmt.Errorf("%w: amount has no currency", money.ErrUninitialized)
	}
	if !p.Amount.IsPositive() {
		return nil, fmt.Errorf("%w: got %s", ErrOpeningRequiresPositiveAmount, p.Amount)
	}

	now := p.Now.UTC()
	return &Transaction{
		id:                     p.ID,
		origin:                 OriginInternal,
		kind:                   KindOpening,
		state:                  StateProcessed, // terminal direto: não há etapa assíncrona
		playerID:               p.PlayerID,
		walletID:               p.WalletID,
		amount:                 p.Amount,
		resultBalance:          SomeMoney(p.Amount),
		referenceTransactionID: NoID(),
		createdAt:              now,
		updatedAt:              now,
		processedAt:            now,
	}, nil
}

// RehydrateParams reúne o ESTADO COMPLETO lido do banco.
type RehydrateParams struct {
	ID                             ID
	Origin                         Origin
	Kind                           Kind
	State                          State
	PlayerID                       wallet.PlayerID
	WalletID                       wallet.ID
	Amount                         money.Money
	ProviderID                     ProviderID
	ExternalTransactionID          ExternalTransactionID
	IdempotencyKey                 IdempotencyKey
	PayloadHash                    PayloadHash
	RoundID                        string
	GameID                         string
	CorrelationID                  string
	ReferenceExternalTransactionID ExternalTransactionID
	ReferenceTransactionID         OptionalID
	FailureCode                    FailureCode
	ResultBalance                  OptionalMoney
	Attempts                       int
	NextRetryAt                    time.Time
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
	ProcessedAt                    time.Time
}

// Rehydrate reconstrói a transação do estado persistido. NÃO reaplica transições
// nem emite eventos (§6): apenas valida a consistência estrutural do que foi lido,
// falhando alto diante de estado impossível (kind, amount ou origem incoerentes).
func Rehydrate(p RehydrateParams) (*Transaction, error) {
	if err := validateID(p.ID); err != nil {
		return nil, err
	}
	if !p.Kind.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, p.Kind)
	}
	if !p.State.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidState, p.State)
	}
	if !p.Origin.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInconsistentOrigin, p.Origin)
	}
	if p.Amount.Currency().IsZero() {
		return nil, fmt.Errorf("%w: amount has no currency", money.ErrUninitialized)
	}
	if err := validateAmountForKind(p.Kind, p.Amount); err != nil {
		return nil, err
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("%w: timestamps", ErrInvalidTimestamp)
	}
	if p.Attempts < 0 {
		return nil, fmt.Errorf("%w: %d", ErrNegativeAttempts, p.Attempts)
	}
	if p.FailureCode != "" && !p.FailureCode.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidFailureCode, p.FailureCode)
	}
	// Coerência entre origem e metadados (§6.3).
	if p.Origin == OriginInternal && p.Kind != KindOpening {
		return nil, fmt.Errorf("%w: internal origin only allowed for OPENING, got %s", ErrInconsistentOrigin, p.Kind)
	}
	if p.Origin == OriginExternal {
		if isBlank(string(p.ProviderID)) || isBlank(string(p.ExternalTransactionID)) ||
			isBlank(string(p.IdempotencyKey)) || isBlank(string(p.PayloadHash)) {
			return nil, ErrMissingExternalMetadata
		}
	}

	return &Transaction{
		id:                             p.ID,
		origin:                         p.Origin,
		kind:                           p.Kind,
		state:                          p.State,
		playerID:                       p.PlayerID,
		walletID:                       p.WalletID,
		amount:                         p.Amount,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		correlationID:                  p.CorrelationID,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		referenceTransactionID:         p.ReferenceTransactionID,
		failureCode:                    p.FailureCode,
		resultBalance:                  p.ResultBalance,
		attempts:                       p.Attempts,
		nextRetryAt:                    p.NextRetryAt.UTC(),
		createdAt:                      p.CreatedAt.UTC(),
		updatedAt:                      p.UpdatedAt.UTC(),
		processedAt:                    p.ProcessedAt.UTC(),
	}, nil
}

// validateAmountForKind aplica a regra de valor por tipo (§7): LOSS exige zero; os
// demais externos exigem valor estritamente positivo.
func validateAmountForKind(kind Kind, amount money.Money) error {
	if kind == KindLoss {
		if !amount.IsZero() {
			return fmt.Errorf("%w: got %s", ErrLossAmountNotZero, amount)
		}
		return nil
	}
	if !amount.IsPositive() {
		return fmt.Errorf("%w: kind=%s requires a positive amount, got %s", ErrInvalidAmount, kind, amount)
	}
	return nil
}

func validateID(id ID) error {
	if _, err := NewID(string(id)); err != nil {
		return err
	}
	return nil
}

// Transições de estado são os únicos caminhos que alteram `state`; todas passam
// por State.validateTransition.

// MarkProcessed leva PENDING ou PENDING_REFERENCE para PROCESSED (§6.3),
// registrando o saldo observado no processamento (usado no replay do §9).
func (t *Transaction) MarkProcessed(resultBalance money.Money, now time.Time) error {
	if t == nil || t.id == "" {
		return ErrUninitialized
	}
	if err := t.state.validateTransition(StateProcessed); err != nil {
		return err
	}
	if resultBalance.Currency().IsZero() {
		return fmt.Errorf("%w: result balance has no currency", money.ErrUninitialized)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: transition time", ErrInvalidTimestamp)
	}

	ts := now.UTC()
	t.state = StateProcessed
	t.resultBalance = SomeMoney(resultBalance)
	t.failureCode = "" // sucesso limpa qualquer código de falha anterior
	t.processedAt = ts
	t.updatedAt = ts
	t.nextRetryAt = time.Time{} // não há mais o que reagendar
	return nil
}

// MarkRejected leva a transação a REJECTED (terminal) com código de falha estável
// (§7). É o caminho de rejeição de negócio, não de erro de I/O.
func (t *Transaction) MarkRejected(code FailureCode, now time.Time) error {
	return t.markTerminalFailure(StateRejected, code, now)
}

// MarkFailed leva a transação a FAILED (terminal): falha permanente de
// infraestrutura (§6.3). Diferente de REJECTED, não é regra de negócio.
func (t *Transaction) MarkFailed(code FailureCode, now time.Time) error {
	return t.markTerminalFailure(StateFailed, code, now)
}

// markTerminalFailure concentra a lógica compartilhada por REJECTED e FAILED.
func (t *Transaction) markTerminalFailure(target State, code FailureCode, now time.Time) error {
	if t == nil || t.id == "" {
		return ErrUninitialized
	}
	if !code.IsValid() {
		return fmt.Errorf("%w: %q", ErrInvalidFailureCode, code)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: transition time", ErrInvalidTimestamp)
	}
	if err := t.state.validateTransition(target); err != nil {
		return err
	}

	ts := now.UTC()
	t.state = target
	t.failureCode = code
	t.updatedAt = ts
	t.processedAt = ts
	t.nextRetryAt = time.Time{}
	return nil
}

// MarkPendingReference leva PENDING -> PENDING_REFERENCE (§6.3/§7), agendando a
// próxima tentativa e incrementando `attempts`. Serve tanto para a primeira espera
// quanto para cada reagendamento, permitindo backoff exponencial até o teto.
func (t *Transaction) MarkPendingReference(code FailureCode, nextRetryAt time.Time, now time.Time) error {
	if t == nil || t.id == "" {
		return ErrUninitialized
	}
	if !code.IsValid() {
		return fmt.Errorf("%w: %q", ErrInvalidFailureCode, code)
	}
	if now.IsZero() || nextRetryAt.IsZero() {
		return fmt.Errorf("%w: pending reference schedule", ErrInvalidTimestamp)
	}
	if err := t.state.validateTransition(StatePendingReference); err != nil {
		return err
	}

	ts := now.UTC()
	t.state = StatePendingReference
	t.failureCode = code
	t.attempts++
	t.nextRetryAt = nextRetryAt.UTC()
	t.updatedAt = ts
	return nil
}

// AttachResolvedReference registra a referência interna resolvida (§6.3). Não muda
// estado: é informação de auditoria e base para a regra de reversão única.
func (t *Transaction) AttachResolvedReference(referenceID ID, now time.Time) error {
	if t == nil || t.id == "" {
		return ErrUninitialized
	}
	if err := validateID(referenceID); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: reference resolution time", ErrInvalidTimestamp)
	}
	t.referenceTransactionID = SomeID(referenceID)
	t.updatedAt = now.UTC()
	return nil
}

// Acessores (somente leitura): único jeito de observar o agregado de fora do pacote.

func (t *Transaction) ID() ID                    { return t.id }
func (t *Transaction) Origin() Origin            { return t.origin }
func (t *Transaction) Kind() Kind                { return t.kind }
func (t *Transaction) State() State              { return t.state }
func (t *Transaction) PlayerID() wallet.PlayerID { return t.playerID }
func (t *Transaction) WalletID() wallet.ID       { return t.walletID }
func (t *Transaction) Amount() money.Money       { return t.amount }
func (t *Transaction) ProviderID() ProviderID    { return t.providerID }
func (t *Transaction) ExternalTransactionID() ExternalTransactionID {
	return t.externalTransactionID
}
func (t *Transaction) IdempotencyKey() IdempotencyKey { return t.idempotencyKey }
func (t *Transaction) PayloadHash() PayloadHash       { return t.payloadHash }
func (t *Transaction) RoundID() string                { return t.roundID }
func (t *Transaction) GameID() string                 { return t.gameID }

// CorrelationID devolve o identificador de correlação do pedido original (§12).
func (t *Transaction) CorrelationID() string { return t.correlationID }
func (t *Transaction) ReferenceExternalTransactionID() ExternalTransactionID {
	return t.referenceExternalTransactionID
}
func (t *Transaction) ReferenceTransactionID() OptionalID { return t.referenceTransactionID }
func (t *Transaction) FailureCode() FailureCode           { return t.failureCode }
func (t *Transaction) ResultBalance() OptionalMoney       { return t.resultBalance }
func (t *Transaction) Attempts() int                      { return t.attempts }
func (t *Transaction) NextRetryAt() time.Time             { return t.nextRetryAt }
func (t *Transaction) CreatedAt() time.Time               { return t.createdAt }
func (t *Transaction) UpdatedAt() time.Time               { return t.updatedAt }
func (t *Transaction) ProcessedAt() time.Time             { return t.processedAt }

// IsTerminal informa se a transação chegou a um estado final (§6.3).
func (t *Transaction) IsTerminal() bool { return t.state.IsTerminal() }

// HasResolvedReference informa se a referência interna já foi vinculada.
func (t *Transaction) HasResolvedReference() bool { return t.referenceTransactionID.Valid }
