package event

import (
	"fmt"
	"time"
)

var (
	ErrInvalidEvent = fmt.Errorf("event: invalid event")

	// ErrMissingEventID / ErrMissingAggregateID / ErrMissingCorrelationID: campos
	// obrigatórios do §11 (correlationId amarra o evento à requisição original).
	ErrMissingEventID       = fmt.Errorf("event: missing event id")
	ErrMissingAggregateID   = fmt.Errorf("event: missing aggregate id")
	ErrMissingCorrelationID = fmt.Errorf("event: missing correlation id")

	ErrInvalidTimestamp = fmt.Errorf("event: invalid timestamp")
	ErrUnknownEventType = fmt.Errorf("event: unknown event type")
	ErrMissingEventType = fmt.Errorf("event: missing event type")
)

// Tipos de evento do §11: lista fechada aceita pela coluna `event_type` (CHECK no schema).
const (
	// TypeWagerTransactionProcessed: conclusão bem-sucedida, incluindo LOSS.
	TypeWagerTransactionProcessed = "WagerTransactionProcessed"

	// TypeWagerTransactionRejected: rejeição definitiva por regra de negócio.
	TypeWagerTransactionRejected = "WagerTransactionRejected"

	// TypeWalletBalanceChanged: alteração efetiva de saldo (LOSS não produz, §7).
	TypeWalletBalanceChanged = "WalletBalanceChanged"

	// TypeWagerTransactionPendingReference: espera pela referência.
	TypeWagerTransactionPendingReference = "WagerTransactionPendingReference"
)

// Versão de schema fixada pelo construtor do evento (§11).
const (
	VersionV1 = 1
)

var validEventTypes = map[string]struct{}{
	TypeWagerTransactionProcessed:        {},
	TypeWagerTransactionRejected:         {},
	TypeWalletBalanceChanged:             {},
	TypeWagerTransactionPendingReference: {},
}

// Envelope é o header compartilhado por todos os eventos (§11). É exportado porque
// os quatro eventos o incorporam (embed); tags em camelCase casam com o contrato.
type Envelope struct {
	EventID       string `json:"eventId"`
	EventType     string `json:"eventType"`
	AggregateID   string `json:"aggregateId"`
	CorrelationID string `json:"correlationId"`
	// CausationId é opcional (§11); `omitempty` o remove do JSON quando vazio.
	CausationID string    `json:"causationId,omitempty"`
	OccurredAt  time.Time `json:"occurredAt"`
	Version     int       `json:"version"`
}

// envelopeParams não é exportado: só os construtores deste pacote montam envelopes,
// e eles fixam EventType/Version internamente.
type envelopeParams struct {
	EventID       string
	EventType     string
	AggregateID   string
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

// newEnvelope valida e monta o header. Toda validação estrutural do §11 vive aqui.
func newEnvelope(p envelopeParams) (Envelope, error) {
	if isBlank(p.EventType) {
		return Envelope{}, ErrMissingEventType
	}
	if _, ok := validEventTypes[p.EventType]; !ok {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownEventType, p.EventType)
	}
	if isBlank(p.EventID) {
		return Envelope{}, ErrMissingEventID
	}
	if isBlank(p.AggregateID) {
		return Envelope{}, ErrMissingAggregateID
	}
	if isBlank(p.CorrelationID) {
		return Envelope{}, ErrMissingCorrelationID
	}
	if p.OccurredAt.IsZero() {
		return Envelope{}, ErrInvalidTimestamp
	}

	return Envelope{
		EventID:       p.EventID,
		EventType:     p.EventType,
		AggregateID:   p.AggregateID,
		CorrelationID: p.CorrelationID,
		CausationID:   p.CausationID,
		// Sempre UTC: o contrato usa RFC 3339 e não varia com o fuso do host.
		OccurredAt: p.OccurredAt.UTC(),
		Version:    VersionV1,
	}, nil
}

func (e Envelope) HasCausation() bool { return e.CausationID != "" }

// isBlank informa se a string é vazia ou só espaços.
func isBlank(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return false
		}
	}
	return true
}
