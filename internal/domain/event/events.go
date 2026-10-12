package event

import (
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var (
	ErrMissingTransactionID = fmt.Errorf("event: missing transaction id")
	ErrMissingWalletID      = fmt.Errorf("event: missing wallet id")
	ErrMissingMoney         = fmt.Errorf("event: missing money value")
)

// WagerTransactionProcessedData é o `data` do evento de conclusão.
type WagerTransactionProcessedData struct {
	TransactionID         string      `json:"transactionId"`
	WalletID              string      `json:"walletId"`
	PlayerID              string      `json:"playerId"`
	ProviderID            string      `json:"providerId"`
	ExternalTransactionID string      `json:"externalTransactionId"`
	Kind                  string      `json:"kind"`
	Money                 money.Money `json:"money"`
	ResultBalance         money.Money `json:"resultBalance"`
}

// WagerTransactionProcessed é o evento de conclusão (embed promove o header ao topo do JSON).
type WagerTransactionProcessed struct {
	Envelope
	Data WagerTransactionProcessedData `json:"data"`
}

// NewWagerTransactionProcessed constrói o evento da transação processada e do saldo
// observado. LOSS também gera este evento (§11), mesmo sem mover saldo — por isso
// ResultBalance é obrigatório: registra o saldo no momento da conclusão.
func NewWagerTransactionProcessed(
	eventID string,
	correlationID string,
	causationID string,
	tx *transaction.Transaction,
	resultBalance money.Money,
	occurredAt time.Time,
) (WagerTransactionProcessed, error) {
	if tx == nil {
		return WagerTransactionProcessed{}, ErrInvalidEvent
	}
	if resultBalance.Currency().IsZero() {
		return WagerTransactionProcessed{}, fmt.Errorf("%w: resultBalance", ErrMissingMoney)
	}

	envelope, err := newEnvelope(envelopeParams{
		EventID:       eventID,
		EventType:     TypeWagerTransactionProcessed,
		AggregateID:   tx.ID().String(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt,
	})
	if err != nil {
		return WagerTransactionProcessed{}, err
	}

	return WagerTransactionProcessed{
		Envelope: envelope,
		Data: WagerTransactionProcessedData{
			TransactionID:         tx.ID().String(),
			WalletID:              string(tx.WalletID()),
			PlayerID:              string(tx.PlayerID()),
			ProviderID:            tx.ProviderID().String(),
			ExternalTransactionID: tx.ExternalTransactionID().String(),
			Kind:                  tx.Kind().String(),
			Money:                 tx.Amount(),
			ResultBalance:         resultBalance,
		},
	}, nil
}

// WagerTransactionRejectedData é o `data` do evento de rejeição.
type WagerTransactionRejectedData struct {
	TransactionID         string                  `json:"transactionId"`
	WalletID              string                  `json:"walletId"`
	PlayerID              string                  `json:"playerId"`
	ProviderID            string                  `json:"providerId"`
	ExternalTransactionID string                  `json:"externalTransactionId"`
	Kind                  string                  `json:"kind"`
	Money                 money.Money             `json:"money"`
	FailureCode           transaction.FailureCode `json:"failureCode"`
	Correctable           bool                    `json:"correctable"`
}

// WagerTransactionRejected é o evento de rejeição por regra de negócio.
type WagerTransactionRejected struct {
	Envelope
	Data WagerTransactionRejectedData `json:"data"`
}

// NewWagerTransactionRejected constrói o evento de rejeição. `Correctable` é derivado
// do código de falha (§7) e materializado no payload, para o consumidor não precisar
// conhecer a tabela de códigos.
func NewWagerTransactionRejected(
	eventID string,
	correlationID string,
	causationID string,
	tx *transaction.Transaction,
	failureCode transaction.FailureCode,
	occurredAt time.Time,
) (WagerTransactionRejected, error) {
	if tx == nil {
		return WagerTransactionRejected{}, ErrInvalidEvent
	}
	if !failureCode.IsValid() {
		return WagerTransactionRejected{}, fmt.Errorf("%w: %q", transaction.ErrInvalidFailureCode, failureCode)
	}

	envelope, err := newEnvelope(envelopeParams{
		EventID:       eventID,
		EventType:     TypeWagerTransactionRejected,
		AggregateID:   tx.ID().String(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt,
	})
	if err != nil {
		return WagerTransactionRejected{}, err
	}

	return WagerTransactionRejected{
		Envelope: envelope,
		Data: WagerTransactionRejectedData{
			TransactionID:         tx.ID().String(),
			WalletID:              string(tx.WalletID()),
			PlayerID:              string(tx.PlayerID()),
			ProviderID:            tx.ProviderID().String(),
			ExternalTransactionID: tx.ExternalTransactionID().String(),
			Kind:                  tx.Kind().String(),
			Money:                 tx.Amount(),
			FailureCode:           failureCode,
			Correctable:           failureCode.IsCorrectable(),
		},
	}, nil
}

// WalletBalanceChangedData é o `data` do evento de mudança de saldo, com os campos
// exigidos pelo §11: walletId, transactionId, direction, money, balanceBefore,
// balanceAfter e walletVersion.
type WalletBalanceChangedData struct {
	WalletID      string           `json:"walletId"`
	TransactionID string           `json:"transactionId"`
	Direction     wallet.Direction `json:"direction"`
	Money         money.Money      `json:"money"`
	BalanceBefore money.Money      `json:"balanceBefore"`
	BalanceAfter  money.Money      `json:"balanceAfter"`
	WalletVersion int64            `json:"walletVersion"`
}

// WalletBalanceChanged é emitido somente quando houve movimentação (LOSS não o produz, §7).
type WalletBalanceChanged struct {
	Envelope
	Data WalletBalanceChangedData `json:"data"`
}

// NewWalletBalanceChanged constrói o evento a partir do movimento da carteira: receber
// wallet.Movement (e não campos soltos) garante que os valores do evento são
// exatamente os aplicados. O aggregateId é a carteira.
func NewWalletBalanceChanged(
	eventID string,
	correlationID string,
	causationID string,
	walletID wallet.ID,
	transactionID transaction.ID,
	movement wallet.Movement,
	occurredAt time.Time,
) (WalletBalanceChanged, error) {
	if isBlank(string(walletID)) {
		return WalletBalanceChanged{}, ErrMissingWalletID
	}
	if isBlank(string(transactionID)) {
		return WalletBalanceChanged{}, ErrMissingTransactionID
	}
	if movement.Amount().Currency().IsZero() {
		return WalletBalanceChanged{}, fmt.Errorf("%w: movement amount", ErrMissingMoney)
	}

	envelope, err := newEnvelope(envelopeParams{
		EventID:       eventID,
		EventType:     TypeWalletBalanceChanged,
		AggregateID:   string(walletID),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt,
	})
	if err != nil {
		return WalletBalanceChanged{}, err
	}

	return WalletBalanceChanged{
		Envelope: envelope,
		Data: WalletBalanceChangedData{
			WalletID:      string(walletID),
			TransactionID: string(transactionID),
			Direction:     movement.Direction(),
			Money:         movement.Amount(),
			BalanceBefore: movement.BalanceBefore(),
			BalanceAfter:  movement.BalanceAfter(),
			WalletVersion: movement.WalletVersion(),
		},
	}, nil
}

// WagerTransactionPendingReferenceData é o `data` do evento de espera.
type WagerTransactionPendingReferenceData struct {
	TransactionID                  string `json:"transactionId"`
	WalletID                       string `json:"walletId"`
	ProviderID                     string `json:"providerId"`
	ExternalTransactionID          string `json:"externalTransactionId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
	Kind                           string `json:"kind"`
	// Attempts/NextRetryAt expõem o backoff em curso ao consumidor (§7).
	Attempts    int       `json:"attempts"`
	NextRetryAt time.Time `json:"nextRetryAt"`
}

// WagerTransactionPendingReference é o evento emitido quando a operação entra
// (ou permanece) em PENDING_REFERENCE.
type WagerTransactionPendingReference struct {
	Envelope
	Data WagerTransactionPendingReferenceData `json:"data"`
}

// NewWagerTransactionPendingReference constrói o evento de espera por referência.
func NewWagerTransactionPendingReference(
	eventID string,
	correlationID string,
	causationID string,
	tx *transaction.Transaction,
	occurredAt time.Time,
) (WagerTransactionPendingReference, error) {
	if tx == nil {
		return WagerTransactionPendingReference{}, ErrInvalidEvent
	}
	if tx.NextRetryAt().IsZero() {
		// Sem agendamento não há espera durável.
		return WagerTransactionPendingReference{}, ErrInvalidTimestamp
	}

	envelope, err := newEnvelope(envelopeParams{
		EventID:       eventID,
		EventType:     TypeWagerTransactionPendingReference,
		AggregateID:   tx.ID().String(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		OccurredAt:    occurredAt,
	})
	if err != nil {
		return WagerTransactionPendingReference{}, err
	}

	return WagerTransactionPendingReference{
		Envelope: envelope,
		Data: WagerTransactionPendingReferenceData{
			TransactionID:                  tx.ID().String(),
			WalletID:                       string(tx.WalletID()),
			ProviderID:                     tx.ProviderID().String(),
			ExternalTransactionID:          tx.ExternalTransactionID().String(),
			ReferenceExternalTransactionID: tx.ReferenceExternalTransactionID().String(),
			Kind:                           tx.Kind().String(),
			Attempts:                       tx.Attempts(),
			NextRetryAt:                    tx.NextRetryAt().UTC(),
		},
	}, nil
}
