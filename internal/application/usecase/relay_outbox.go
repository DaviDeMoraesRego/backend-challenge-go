package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

var _ ports.RelayOutbox = (*RelayOutboxService)(nil)

// RelayOutboxConfig reúne a política do relay.
type RelayOutboxConfig struct {
	// BatchSize é quantos eventos uma rodada reserva de uma vez.
	BatchSize int
	// Lease é por quanto tempo um evento fica reservado antes de poder ser
	// reivindicado por outra instância em caso de morte do publisher.
	Lease time.Duration
	// BaseBackoff é a base do backoff exponencial em caso de falha de publicação.
	BaseBackoff time.Duration
	// MaxAttempts é o teto de tentativas antes de desistir do evento.
	MaxAttempts int
}

// comDefaults preenche a política com valores seguros quando não informada.
func (c RelayOutboxConfig) comDefaults() RelayOutboxConfig {
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.Lease <= 0 {
		// Lease maior que o tempo típico de publicação: curto faria duas instâncias
		// publicarem o mesmo evento; longo atrasaria a recuperação de publisher morto.
		c.Lease = 30 * time.Second
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	return c
}

// RelayOutboxService implementa a porta `ports.RelayOutbox`.
type RelayOutboxService struct {
	uow         ports.UnitOfWork
	publisher   ports.EventPublisher
	clock       ports.Clock
	batchSize   int
	lease       time.Duration
	baseBackoff time.Duration
	maxAttempts int
}

func NewRelayOutboxService(
	uow ports.UnitOfWork,
	publisher ports.EventPublisher,
	clock ports.Clock,
	cfg RelayOutboxConfig,
) *RelayOutboxService {
	resolved := cfg.comDefaults()
	return &RelayOutboxService{
		uow:         uow,
		publisher:   publisher,
		clock:       clock,
		batchSize:   resolved.BatchSize,
		lease:       resolved.Lease,
		baseBackoff: resolved.BaseBackoff,
		maxAttempts: resolved.MaxAttempts,
	}
}

// Execute roda uma rodada de publicação. Devolve um RESUMO (não erro) para falhas
// individuais: um evento não entregue é caso de negócio do relay (reagendar), não
// falha da rodada — o resumo alimenta as métricas do §12.
func (s *RelayOutboxService) Execute(ctx context.Context) (ports.RelayOutboxResult, error) {
	now := s.clock.Now()

	// --- FASE 1: reserva (transação curta) ---
	var claimed []ports.OutboxRecord
	err := s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		// O lease (política do caso de uso) vira o novo `next_attempt_at`: o lock do
		// banco é liberado já (nenhuma transação aberta durante a publicação) e a
		// recuperação de trabalho abandonado passa a depender do vencimento do lease.
		leaseUntil := now.Add(s.lease)

		records, err := scope.Outbox().ClaimPending(txCtx, now, leaseUntil, s.batchSize)
		if err != nil {
			return err
		}
		claimed = records
		return nil
	})
	if err != nil {
		// Falha de BANCO: transitória. A rodada inteira falha e será repetida.
		return ports.RelayOutboxResult{}, fmt.Errorf("%w: claiming outbox records: %v", ports.ErrUnavailable, err)
	}

	result := ports.RelayOutboxResult{Claimed: len(claimed)}
	if len(claimed) == 0 {
		return result, nil
	}

	// --- FASE 2: publicação (FORA de qualquer transação) ---
	//
	// Publicar fora do `uow.Execute` evita segurar lock de banco durante I/O de rede.
	// O preço é confirmar em transações separadas — a janela de falha aceita pelo
	// modelo at-least-once do §11 (republicação preserva o eventId).
	var maxLag time.Duration
	for _, record := range claimed {
		if lag := now.Sub(record.OccurredAt); lag > maxLag {
			maxLag = lag
		}

		published, err := s.publishAndConfirm(ctx, record, now)
		if err != nil {
			// Erro do BANCO ao confirmar/reagendar (o broker já foi tratado no método).
			return result, fmt.Errorf("%w: confirming outbox publication: %v", ports.ErrUnavailable, err)
		}

		// O contador vem do RETORNO do método, não de campos do registro: `claimed`
		// carrega valores, então mutações internas não aparecem no laço.
		switch {
		case published:
			result.Published++
		case record.Attempts+1 >= s.maxAttempts:
			result.Failed++
		default:
			result.Rescheduled++
		}
	}
	result.MaxLag = maxLag

	return result, nil
}

// publishAndConfirm entrega um registro e grava o desfecho. Devolve `published` porque
// o registro chega por VALOR: mutações internas morreriam na saída da função.
func (s *RelayOutboxService) publishAndConfirm(ctx context.Context, record ports.OutboxRecord, now time.Time) (bool, error) {
	// MessageGroupId = id do agregado: a ordem é preservada por agregado (carteira em
	// eventos de saldo, transação nos demais). Agregados diferentes podem intercalar.
	groupID := record.AggregateID

	publishErr := s.publisher.Publish(ctx, record, groupID)

	published := false
	err := s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		if publishErr == nil {
			if err := scope.Outbox().MarkPublished(txCtx, record.EventID, now); err != nil {
				return err
			}
			published = true
			return nil
		}

		attempts := record.Attempts + 1
		if attempts >= s.maxAttempts {
			// Teto atingido: desiste (dead-letter) em vez de reagendar para sempre.
			return scope.Outbox().MarkDead(txCtx, record.EventID, now)
		}

		nextAttemptAt := now.Add(exponentialBackoff(s.baseBackoff, attempts))
		return scope.Outbox().Reschedule(txCtx, record.EventID, attempts, nextAttemptAt)
	})
	return published, err
}
