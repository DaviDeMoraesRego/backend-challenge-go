package usecase

import (
	"encoding/json"
	"fmt"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/event"
)

// Tipos de agregado gravados em `aggregate_type`: o consumidor usa o campo para
// interpretar `aggregate_id` (carteira em evento de saldo, transação nos demais).
const (
	aggregateTypeTransaction = "WagerTransaction"
	aggregateTypeWallet      = "Wallet"
)

// newOutboxRecord monta o registro a partir do envelope e do payload. Serializar AQUI
// (antes de gravar) garante o snapshot imutável do §11: o que está no banco é, por
// construção, o que será publicado, mesmo em republicação tardia. O `payload` é o
// evento completo (envelope + data), como o pacote `event` já produz.
func newOutboxRecord(envelope event.Envelope, payload any, aggregateType string) (ports.OutboxRecord, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Só por bug de tipo (campo não serializável); erro em vez de panic.
		return ports.OutboxRecord{}, fmt.Errorf("usecase: cannot encode %s payload: %w", envelope.EventType, err)
	}

	return ports.OutboxRecord{
		EventID:       envelope.EventID,
		AggregateType: aggregateType,
		AggregateID:   envelope.AggregateID,
		EventType:     envelope.EventType,
		EventVersion:  envelope.Version,
		Payload:       encoded,
		CorrelationID: envelope.CorrelationID,
		CausationID:   envelope.CausationID,
		OccurredAt:    envelope.OccurredAt,
		// `NextAttemptAt` no instante da ocorrência: o relay pega o evento assim
		// que rodar. Zero aqui significaria "vencido desde sempre", o que também
		// funcionaria, mas esconde a intenção de publicar imediatamente.
		NextAttemptAt: envelope.OccurredAt,
	}, nil
}

// transactionEventRecord é o atalho para eventos cujo agregado é a TRANSAÇÃO.
func transactionEventRecord(envelope event.Envelope, payload any) (ports.OutboxRecord, error) {
	return newOutboxRecord(envelope, payload, aggregateTypeTransaction)
}

// walletEventRecord é o atalho para eventos cujo agregado é a CARTEIRA.
func walletEventRecord(envelope event.Envelope, payload any) (ports.OutboxRecord, error) {
	return newOutboxRecord(envelope, payload, aggregateTypeWallet)
}
