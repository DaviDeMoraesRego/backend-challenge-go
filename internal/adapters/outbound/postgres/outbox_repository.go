package postgres

import (
	"context"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

type outboxRepository struct {
	q querier
}

var _ ports.OutboxRepository = (*outboxRepository)(nil)

// Append grava os eventos. Como já estamos dentro de uma transação aberta, o ganho é
// de latência (round trips); a atomicidade vem da transação do chamador. Um erro em
// qualquer INSERT aborta a transação inteira — evento faltando seria pior que operação desfeita.
func (r *outboxRepository) Append(ctx context.Context, records ...ports.OutboxRecord) error {
	if len(records) == 0 {
		return nil
	}

	const query = `
		INSERT INTO outbox_events (
			id, aggregate_type, aggregate_id, event_type, event_version,
			payload, correlation_id, causation_id, occurred_at,
			attempts, next_attempt_at
		) VALUES ($1::uuid, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11)`

	batch := &batchBuilder{q: r.q}
	for _, record := range records {
		batch.add(ctx, query,
			record.EventID,
			record.AggregateType,
			record.AggregateID,
			record.EventType,
			record.EventVersion,
			record.Payload, // []byte -> jsonb
			nullString(record.CorrelationID),
			nullString(record.CausationID),
			record.OccurredAt,
			record.Attempts,
			record.NextAttemptAt,
		)
	}
	return batch.flush(ctx)
}

// ClaimPending reserva um lote pendente (vencido, mais atrasado primeiro) e aplica o
// LEASE:
//
//	SELECT id FROM outbox_events
//	 WHERE published_at IS NULL AND next_attempt_at <= $1
//	 ORDER BY next_attempt_at LIMIT $2 FOR UPDATE SKIP LOCKED
//	-- UPDATE ... SET next_attempt_at = leaseUntil ... RETURNING ...
//
// FOR UPDATE SKIP LOCKED faz o publisher PULAR linhas já bloqueadas por outro, sem
// esperar — é o que permite múltiplos publishers disputarem a mesma tabela (§11). Em
// vez de MANTER o lock durante a publicação (prenderia a linha pelo tempo de rede),
// empurramos next_attempt_at para leaseUntil e cometemos: a linha fica "reservada por
// tempo" e, se o processo morrer, o lease vence e outra instância assume (§11).
func (r *outboxRepository) ClaimPending(ctx context.Context, now, leaseUntil time.Time, limit int) ([]ports.OutboxRecord, error) {
	const query = `
		WITH claimed AS (
			SELECT id
			  FROM outbox_events
			 WHERE published_at IS NULL
			   AND dead_at IS NULL
			   AND next_attempt_at <= $1
			 ORDER BY next_attempt_at
			 LIMIT $2
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events AS o
		   SET next_attempt_at = $3
		  FROM claimed
		 WHERE o.id = claimed.id
		RETURNING
			o.id::text, o.aggregate_type, o.aggregate_id::text, o.event_type, o.event_version,
			o.payload, o.correlation_id, o.causation_id, o.occurred_at,
			o.attempts, o.next_attempt_at, o.published_at`

	rows, err := r.q.Query(ctx, query, now, limit, leaseUntil)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var records []ports.OutboxRecord
	for rows.Next() {
		var (
			record        ports.OutboxRecord
			correlationID *string
			causationID   *string
			publishedAt   *time.Time
			nextAttemptAt time.Time
		)
		if err := rows.Scan(
			&record.EventID, &record.AggregateType, &record.AggregateID,
			&record.EventType, &record.EventVersion, &record.Payload,
			&correlationID, &causationID, &record.OccurredAt,
			&record.Attempts, &nextAttemptAt, &publishedAt,
		); err != nil {
			return nil, mapError(err)
		}
		record.CorrelationID = derefString(correlationID)
		record.CausationID = derefString(causationID)
		record.NextAttemptAt = nextAttemptAt
		record.PublishedAt = derefTime(publishedAt)
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return records, nil
}

// MarkPublished confirma a publicação, chamado DEPOIS de o broker aceitar (§5.4). A
// janela entre publicar e confirmar é aceita: se o processo morrer aí, o evento é
// republicado com o MESMO event_id (§11) e o consumidor deduplica.
func (r *outboxRepository) MarkPublished(ctx context.Context, eventID string, publishedAt time.Time) error {
	const query = `
		UPDATE outbox_events
		   SET published_at = $2
		 WHERE id = $1::uuid`

	tag, err := r.q.Exec(ctx, query, eventID, publishedAt)
	if err != nil {
		return mapError(err)
	}
	return garantirLinhaAfetada(tag)
}

// Reschedule reagenda um evento que falhou, registrando a tentativa.
func (r *outboxRepository) Reschedule(ctx context.Context, eventID string, attempts int, nextAttemptAt time.Time) error {
	const query = `
		UPDATE outbox_events
		   SET attempts = $2, next_attempt_at = $3
		 WHERE id = $1::uuid`

	tag, err := r.q.Exec(ctx, query, eventID, attempts, nextAttemptAt)
	if err != nil {
		return mapError(err)
	}
	return garantirLinhaAfetada(tag)
}

// MarkDead retira do polling um evento que atingiu o teto de tentativas, sem apagar
// a linha (auditoria). Só age sobre eventos ainda não publicados.
func (r *outboxRepository) MarkDead(ctx context.Context, eventID string, deadAt time.Time) error {
	const query = `
		UPDATE outbox_events
		   SET dead_at = $2, next_attempt_at = $2, attempts = attempts + 1
		 WHERE id = $1::uuid AND published_at IS NULL`

	if _, err := r.q.Exec(ctx, query, eventID, deadAt); err != nil {
		return mapError(err)
	}
	return nil // evento já publicado: no-op
}

// batchBuilder acumula comandos e os envia em sequência. Existe porque os
// repositórios dependem de `querier`, que não inclui SendBatch — mantendo-os
// agnósticos entre pool e transação.
type batchBuilder struct {
	q       querier
	queries []string
	args    [][]any
}

func (b *batchBuilder) add(ctx context.Context, query string, args ...any) {
	b.queries = append(b.queries, query)
	b.args = append(b.args, args)
}

func (b *batchBuilder) flush(ctx context.Context) error {
	// Execução sequencial, válida tanto para transação quanto para pool sem cast.
	for i, query := range b.queries {
		if _, err := b.q.Exec(ctx, query, b.args[i]...); err != nil {
			return mapError(err)
		}
	}
	return nil
}
