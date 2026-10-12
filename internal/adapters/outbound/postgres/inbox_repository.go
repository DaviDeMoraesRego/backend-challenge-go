// Inbox (§6.5): PK composta (consumer_name, message_id). Register NÃO usa
// `ON CONFLICT DO NOTHING` de propósito — com DO NOTHING o chamador não saberia
// distinguir mensagem nova de reentrega, e o §10 exige tratar as duas de forma
// diferente. Deixamos a violação de UNIQUE virar ports.ErrAlreadyExists e o caso de
// uso decide.
package postgres

import (
	"context"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

type inboxRepository struct {
	q querier
}

var _ ports.InboxRepository = (*inboxRepository)(nil)

// Register insere a mensagem. A PK composta (consumer_name, message_id) permite que o
// mesmo messageId seja tratado por consumidores diferentes com históricos próprios.
func (r *inboxRepository) Register(ctx context.Context, consumerName, messageID, messageHash string) error {
	const query = `
		INSERT INTO inbox_messages (consumer_name, message_id, message_hash, received_at)
		VALUES ($1, $2, $3, $4)`

	_, err := r.q.Exec(ctx, query, consumerName, messageID, messageHash, time.Now().UTC())
	// Violação de PK -> `ports.ErrAlreadyExists` -> o caso de uso entende
	// "reentrega" e compara o hash (§10).
	return mapError(err)
}

// MarkProcessed grava a conclusão durável, DENTRO da transação do domínio (§6.5): se o
// commit falhar, a marcação some e a mensagem volta — com a idempotência evitando efeito duplicado.
func (r *inboxRepository) MarkProcessed(ctx context.Context, consumerName, messageID string) error {
	const query = `
		UPDATE inbox_messages
		   SET processed_at = $3
		 WHERE consumer_name = $1 AND message_id = $2`

	tag, err := r.q.Exec(ctx, query, consumerName, messageID, time.Now().UTC())
	if err != nil {
		return mapError(err)
	}
	// Marcar mensagem inexistente é bug de fluxo (Register sempre roda antes).
	return garantirLinhaAfetada(tag)
}

func (r *inboxRepository) Find(ctx context.Context, consumerName, messageID string) (*ports.InboxMessage, error) {
	const query = `
		SELECT consumer_name, message_id, message_hash, received_at, processed_at
		  FROM inbox_messages
		 WHERE consumer_name = $1 AND message_id = $2`

	var (
		record      ports.InboxMessage
		processedAt *time.Time // processed_at é NULL enquanto não concluído
	)
	err := r.q.QueryRow(ctx, query, consumerName, messageID).Scan(
		&record.ConsumerName, &record.MessageID, &record.MessageHash,
		&record.ReceivedAt, &processedAt,
	)
	if err != nil {
		return nil, mapError(err)
	}

	// O caso de uso distingue "registrada mas não concluída" de "já concluída" por
	// ProcessedAt.IsZero().
	record.ProcessedAt = derefTime(processedAt)
	return &record, nil
}
