package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/event"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var _ ports.ResolvePendingReferences = (*ResolvePendingReferencesService)(nil)

// resolveOutcome é o desfecho do tratamento de uma pendência (tipo próprio para o
// compilador cobrir o switch de agregação).
type resolveOutcome int

const (
	// outcomeSkipped: já tratada por outra instância ou fora do estado de espera
	// (valor zero, default seguro).
	outcomeSkipped     resolveOutcome = iota
	outcomeResolved                   // referência chegou, reversão aplicada
	outcomeRescheduled                // referência indisponível, reagendada
	outcomeRejected                   // finalizada como REJECTED
)

// ResolvePendingReferencesConfig reúne a política do worker.
type ResolvePendingReferencesConfig struct {
	// BatchSize limita quantas pendências uma rodada pega de uma vez.
	BatchSize int
	// BaseBackoff é a base do backoff exponencial entre tentativas.
	BaseBackoff time.Duration
	// MaxAttempts é o teto de tentativas antes de REJEITAR (§7: "Defina um número
	// máximo de tentativas ou TTL").
	MaxAttempts int
}

// comDefaults preenche a política ausente com valores seguros.
func (c ResolvePendingReferencesConfig) comDefaults() ResolvePendingReferencesConfig {
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	return c
}

type ResolvePendingReferencesService struct {
	uow         ports.UnitOfWork
	clock       ports.Clock
	ids         ports.IDGenerator
	batchSize   int
	baseBackoff time.Duration
	maxAttempts int
}

func NewResolvePendingReferencesService(
	uow ports.UnitOfWork,
	clock ports.Clock,
	ids ports.IDGenerator,
	cfg ResolvePendingReferencesConfig,
) *ResolvePendingReferencesService {
	resolved := cfg.comDefaults()
	return &ResolvePendingReferencesService{
		uow:         uow,
		clock:       clock,
		ids:         ids,
		batchSize:   resolved.BatchSize,
		baseBackoff: resolved.BaseBackoff,
		maxAttempts: resolved.MaxAttempts,
	}
}

// Execute roda uma rodada. Falha isolada por pendência: uma operação com erro não
// aborta as demais (evita "registro envenenado" travando o worker — princípio da DLQ).
func (s *ResolvePendingReferencesService) Execute(ctx context.Context) (ports.ResolvePendingReferencesResult, error) {
	now := s.clock.Now()

	var due []*transaction.Transaction
	err := s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		list, err := scope.Transactions().ListDuePendingReferences(txCtx, now, s.batchSize)
		if err != nil {
			return err
		}
		due = list
		return nil
	})
	if err != nil {
		return ports.ResolvePendingReferencesResult{}, fmt.Errorf("%w: listing pending references: %v", ports.ErrUnavailable, err)
	}

	result := ports.ResolvePendingReferencesResult{Scanned: len(due)}

	for _, pending := range due {
		outcome, err := s.resolveOne(ctx, pending.ID(), now)
		if err != nil {
			result.Failed++
			continue
		}
		switch outcome {
		case outcomeResolved:
			result.Resolved++
		case outcomeRescheduled:
			result.Rescheduled++
		case outcomeRejected:
			result.Rejected++
		case outcomeSkipped:
			// Nada a somar: não é falha nem sucesso.
		}
	}

	return result, nil
}

// resolveOne trata UMA pendência dentro da sua própria transação.
func (s *ResolvePendingReferencesService) resolveOne(
	ctx context.Context,
	id transaction.ID,
	now time.Time,
) (resolveOutcome, error) {
	outcome := outcomeSkipped

	err := s.uow.Execute(ctx, func(txCtx context.Context, scope ports.TxScope) error {
		// Leitura inicial (sem lock) só para descobrir a carteira a travar; quem decide
		// é a releitura pós-lock.
		initial, err := scope.Transactions().FindByID(txCtx, id)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return nil
			}
			return err
		}
		if initial.State() != transaction.StatePendingReference {
			return nil
		}

		// 1) Travamos a CARTEIRA — o ponto de serialização por carteira (§8).
		w, err := scope.Wallets().LockByID(txCtx, initial.WalletID())
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				// Carteira sumiu (não deveria ocorrer); rejeitar é melhor que deixar presa.
				return s.finalizeRejection(txCtx, scope, initial, transaction.FailureWalletNotFound, now, &outcome)
			}
			return err
		}

		// 2) Releitura pós-lock: impede aplicação dupla.
		pending, err := scope.Transactions().FindByID(txCtx, id)
		if err != nil {
			return err
		}
		if pending.State() != transaction.StatePendingReference {
			// Outra instância concluiu enquanto esperávamos o lock: pular (nada foi aplicado).
			return nil
		}

		// 3) A referência chegou?
		reference, err := scope.Transactions().FindByProviderAndExternalID(
			txCtx, pending.ProviderID(), pending.ReferenceExternalTransactionID())
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				return s.rearmOrExpire(txCtx, scope, pending, transaction.FailureReferenceNotFound, now, &outcome)
			}
			return err
		}

		// 4) O estado da referência decide o desfecho.
		switch reference.State() {
		case transaction.StateProcessed:
			// Segue para validação e aplicação.

		case transaction.StateRejected, transaction.StateFailed:
			// A referência terminou sem sucesso: não há o que desfazer (§7).
			return s.finalizeRejection(txCtx, scope, pending, transaction.FailureReferenceNotProcessed, now, &outcome)

		default:
			// Ainda pendente (PENDING/PENDING_REFERENCE): pode concluir depois.
			return s.rearmOrExpire(txCtx, scope, pending, transaction.FailureReferenceStillPending, now, &outcome)
		}

		// 5) Conformidade — as MESMAS regras da entrada original (§7).
		if code := reversalConformanceFailure(intentOfTransaction(pending), reference); code != "" {
			return s.finalizeRejection(txCtx, scope, pending, code, now, &outcome)
		}

		// 6) Reversão duplicada: outra reversão já concluiu sobre esta referência?
		alreadyReversed, err := scope.Transactions().HasSuccessfulReversal(
			txCtx, pending.ProviderID(), pending.ReferenceExternalTransactionID())
		if err != nil {
			return err
		}
		if alreadyReversed {
			return s.finalizeRejection(txCtx, scope, pending, transaction.FailureDuplicateReversal, now, &outcome)
		}

		// 7) Movimento contrário ao original.
		var movement wallet.Movement
		if oppositeMovementFor(reference.Kind()) == wallet.Credit {
			movement, err = w.Credit(pending.Amount(), now)
		} else {
			movement, err = w.Debit(pending.Amount(), now)
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				return s.finalizeRejection(txCtx, scope, pending, transaction.FailureReversalInsufficientFunds, now, &outcome)
			}
		}
		if err != nil {
			return err
		}

		return s.completeResolved(txCtx, scope, w, pending, reference, movement, now, &outcome)
	})

	return outcome, err
}

// completeResolved conclui a pendência com sucesso (§7 + §5.3/§5.4).
func (s *ResolvePendingReferencesService) completeResolved(
	ctx context.Context,
	scope ports.TxScope,
	w *wallet.Wallet,
	pending *transaction.Transaction,
	reference *transaction.Transaction,
	movement wallet.Movement,
	now time.Time,
	outcome *resolveOutcome,
) error {
	// A referência interna resolvida é persistida (§6.3) antes da transição.
	if err := pending.AttachResolvedReference(reference.ID(), now); err != nil {
		return err
	}
	// PENDING_REFERENCE -> PROCESSED é transição válida na máquina de estados.
	if err := pending.MarkProcessed(movement.BalanceAfter(), now); err != nil {
		return err
	}

	entry, err := ledger.NewEntryFromMovement(pending.WalletID(), pending.ID(), movement, now)
	if err != nil {
		return fmt.Errorf("%w: ledger entry: %v", ports.ErrValidation, err)
	}

	correlationID, err := s.correlationFor(ctx, pending)
	if err != nil {
		return err
	}

	processedEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	processed, err := event.NewWagerTransactionProcessed(processedEventID, correlationID, "", pending, movement.BalanceAfter(), now)
	if err != nil {
		return fmt.Errorf("%w: processed event: %v", ports.ErrValidation, err)
	}

	balanceEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	balanceChanged, err := event.NewWalletBalanceChanged(
		balanceEventID, correlationID, processed.Envelope.EventID, w.ID(), pending.ID(), movement, now)
	if err != nil {
		return fmt.Errorf("%w: balance event: %v", ports.ErrValidation, err)
	}

	if err := scope.Wallets().Update(ctx, w); err != nil {
		return err
	}
	if err := scope.Ledger().Append(ctx, entry); err != nil {
		return err
	}
	if err := scope.Transactions().Update(ctx, pending); err != nil {
		return err
	}

	processedRecord, err := transactionEventRecord(processed.Envelope, processed)
	if err != nil {
		return err
	}
	balanceRecord, err := walletEventRecord(balanceChanged.Envelope, balanceChanged)
	if err != nil {
		return err
	}
	if err := scope.Outbox().Append(ctx, processedRecord, balanceRecord); err != nil {
		return err
	}

	*outcome = outcomeResolved
	return nil
}

// finalizeRejection leva a pendência a REJECTED (estado terminal) com evento.
func (s *ResolvePendingReferencesService) finalizeRejection(
	ctx context.Context,
	scope ports.TxScope,
	pending *transaction.Transaction,
	code transaction.FailureCode,
	now time.Time,
	outcome *resolveOutcome,
) error {
	if err := pending.MarkRejected(code, now); err != nil {
		return err
	}
	if err := scope.Transactions().Update(ctx, pending); err != nil {
		return err
	}

	correlationID, err := s.correlationFor(ctx, pending)
	if err != nil {
		return err
	}
	rejectedEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	rejected, err := event.NewWagerTransactionRejected(rejectedEventID, correlationID, "", pending, code, now)
	if err != nil {
		return fmt.Errorf("%w: rejected event: %v", ports.ErrValidation, err)
	}
	record, err := transactionEventRecord(rejected.Envelope, rejected)
	if err != nil {
		return err
	}
	if err := scope.Outbox().Append(ctx, record); err != nil {
		return err
	}

	*outcome = outcomeRejected
	return nil
}

// rearmOrExpire reagenda a espera ou, ao esgotar o teto, REJEITA (§7).
func (s *ResolvePendingReferencesService) rearmOrExpire(
	ctx context.Context,
	scope ports.TxScope,
	pending *transaction.Transaction,
	code transaction.FailureCode,
	now time.Time,
	outcome *resolveOutcome,
) error {
	nextAttempts := pending.Attempts() + 1

	// §7: esgotado o teto, finaliza como REJECTED com REFERENCE_NOT_FOUND (e não o
	// código da última tentativa): o desfecho significa "não apareceu dentro do prazo".
	if nextAttempts > s.maxAttempts {
		return s.finalizeRejection(ctx, scope, pending, transaction.FailureReferenceNotFound, now, outcome)
	}

	nextRetryAt := now.Add(exponentialBackoff(s.baseBackoff, nextAttempts))
	// Auto-transição PENDING_REFERENCE -> PENDING_REFERENCE: rearma a espera e
	// incrementa `attempts` (ver state.go).
	if err := pending.MarkPendingReference(code, nextRetryAt, now); err != nil {
		return err
	}
	if err := scope.Transactions().Update(ctx, pending); err != nil {
		return err
	}

	correlationID, err := s.correlationFor(ctx, pending)
	if err != nil {
		return err
	}
	pendingEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	pendingEvent, err := event.NewWagerTransactionPendingReference(pendingEventID, correlationID, "", pending, now)
	if err != nil {
		return fmt.Errorf("%w: pending event: %v", ports.ErrValidation, err)
	}
	record, err := transactionEventRecord(pendingEvent.Envelope, pendingEvent)
	if err != nil {
		return err
	}
	if err := scope.Outbox().Append(ctx, record); err != nil {
		return err
	}

	*outcome = outcomeRescheduled
	return nil
}

// correlationFor devolve o correlationId da transação, gerando um novo se vazio. O
// fallback cobre linhas gravadas antes do campo existir; o envelope exige
// correlationId não vazio (§11).
func (s *ResolvePendingReferencesService) correlationFor(ctx context.Context, tx *transaction.Transaction) (string, error) {
	if tx.CorrelationID() != "" {
		return tx.CorrelationID(), nil
	}
	generated, err := s.ids.NewID(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: correlation id", ports.ErrUnavailable)
	}
	return generated, nil
}
