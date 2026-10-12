package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/event"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// applyReversal trata REFUND e ROLLBACK. Recebe a carteira já travada (`w`): nenhuma
// outra operação da mesma carteira altera o saldo enquanto decidimos e escrevemos (§8).
func (s *PlaceWagerOperationService) applyReversal(
	ctx context.Context,
	scope ports.TxScope,
	cmd ports.PlaceOperationCommand,
	parsed parsedOperation,
	correlationID string,
	now time.Time,
	w *wallet.Wallet,
	result *ports.PlaceOperationResult,
) error {
	// 1) Resolução da referência por (providerId, referenceExternalTransactionId).
	reference, err := scope.Transactions().FindByProviderAndExternalID(ctx, parsed.providerID, parsed.referenceExternalID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			// §3: a reversão chegou ANTES da transação referenciada — espera legítima,
			// retomada pelo worker de referências (§7).
			return s.pendingReference(ctx, scope, cmd, parsed, correlationID, now,
				transaction.FailureReferenceNotFound, result)
		}
		return err
	}

	// 2) A referência existe — o estado dela decide o desfecho.
	switch reference.State() {
	case transaction.StateProcessed:
		// Caminho feliz: há movimentação a desfazer.

	case transaction.StateRejected, transaction.StateFailed:
		// Terminou sem sucesso: não há efeito a reverter, aguardar seria para sempre
		// -> rejeição definitiva (§7).
		return s.reject(ctx, scope, cmd, parsed, correlationID, now,
			transaction.FailureReferenceNotProcessed, result)

	default:
		// PENDING/PENDING_REFERENCE: ainda pode concluir -> mantém a espera, com código
		// diferente do "não encontrada" porque aqui sabemos que ela existe.
		return s.pendingReference(ctx, scope, cmd, parsed, correlationID, now,
			transaction.FailureReferenceStillPending, result)
	}

	// 3) Conformidade entre intenção e referência (§7). A checagem é compartilhada
	// com o worker de referências: uma reversão retomada precisa aplicar as mesmas
	// regras do primeiro momento, sem cópias que divergem.
	if code := reversalConformanceFailure(intentOf(parsed), reference); code != "" {
		return s.reject(ctx, scope, cmd, parsed, correlationID, now, code, result)
	}

	// 4) Reversão duplicada (§7). A leitura permite um `failureCode` claro; a garantia
	// é o índice único parcial do schema (wtx_reversal_once_per_reference_uniq).
	alreadyReversed, err := scope.Transactions().HasSuccessfulReversal(ctx, parsed.providerID, parsed.referenceExternalID)
	if err != nil {
		return err
	}
	if alreadyReversed {
		return s.reject(ctx, scope, cmd, parsed, correlationID, now,
			transaction.FailureDuplicateReversal, result)
	}

	// 5) Movimento contrário ao original (§7).
	var movement wallet.Movement

	if oppositeMovementFor(reference.Kind()) == wallet.Credit {
		// Desfazer um débito (BET) => devolve o valor: crédito.
		movement, err = w.Credit(parsed.amount, now)
		if err != nil {
			return err
		}
	} else {
		// Desfazer um crédito (WIN/REFUND) => retoma o valor: débito.
		movement, err = w.Debit(parsed.amount, now)
		if err != nil {
			if errors.Is(err, wallet.ErrInsufficientFunds) {
				// §7 exige código DIFERENTE de "aposta sem saldo": aqui o valor já foi
				// gasto, não é falta de saldo para apostar. O provedor reage distinto.
				return s.reject(ctx, scope, cmd, parsed, correlationID, now,
					transaction.FailureReversalInsufficientFunds, result)
			}
			return err
		}
	}

	// 6) Conclui anexando a referência interna resolvida (§6.3).
	return s.completeWithMovement(
		ctx, scope, cmd, parsed, correlationID, now, w, movement,
		transaction.SomeID(reference.ID()),
		result,
	)
}

// oppositeMovementFor devolve a direção que desfaz a operação referenciada:
// BET->CREDIT, WIN/REFUND->DEBIT. Tipos não reversíveis (LOSS, OPENING) já foram
// barrados por AllowsReferenceFrom antes de chegar aqui.
func oppositeMovementFor(referenced transaction.Kind) wallet.Direction {
	switch referenced {
	case transaction.KindBet:
		return wallet.Credit
	case transaction.KindWin, transaction.KindRefund:
		return wallet.Debit
	default:
		// Inalcançável; Debit é conservador (nunca aumenta saldo por engano).
		return wallet.Debit
	}
}

// pendingReference persiste a espera durável por uma referência (§7). A mensagem de
// entrada pode ser concluída assim que a pendência está persistida, porque o worker de
// referências assume a continuidade (§6.5) — daí o markInboxCompleted.
func (s *PlaceWagerOperationService) pendingReference(
	ctx context.Context,
	scope ports.TxScope,
	cmd ports.PlaceOperationCommand,
	parsed parsedOperation,
	correlationID string,
	now time.Time,
	code transaction.FailureCode,
	result *ports.PlaceOperationResult,
) error {
	tx, err := s.newTransactionEntity(ctx, parsed, now)
	if err != nil {
		return err
	}

	// MarkPendingReference incrementa `attempts` (0 -> 1 aqui) e agenda a próxima
	// tentativa com backoff exponencial; o worker usa o mesmo método a cada tentativa.
	nextAttempts := tx.Attempts() + 1
	nextRetryAt := now.Add(exponentialBackoff(s.backoff, nextAttempts))

	if err := tx.MarkPendingReference(code, nextRetryAt, now); err != nil {
		return err
	}

	if err := scope.Transactions().Insert(ctx, tx); err != nil {
		if errors.Is(err, ports.ErrAlreadyExists) {
			return errConcurrentOperationInsert
		}
		return err
	}

	pendingEventID, err := s.ids.NewID(ctx)
	if err != nil {
		return fmt.Errorf("%w: event id", ports.ErrUnavailable)
	}
	pending, err := event.NewWagerTransactionPendingReference(pendingEventID, correlationID, "", tx, now)
	if err != nil {
		return fmt.Errorf("%w: pending reference event: %v", ports.ErrValidation, err)
	}
	record, err := transactionEventRecord(pending.Envelope, pending)
	if err != nil {
		return err
	}
	if err := scope.Outbox().Append(ctx, record); err != nil {
		return err
	}
	if err := s.markInboxCompleted(ctx, scope, cmd); err != nil {
		return err
	}

	// PENDING_REFERENCE é devolvido ao cliente: a operação não foi rejeitada, está
	// registrada e será retomada (§7).
	*result = ports.PlaceOperationResult{
		TransactionID:    tx.ID(),
		State:            tx.State(),
		FailureCode:      code,
		BalanceKnown:     false,
		IdempotentReplay: false,
	}
	return nil
}

// reversalIntent reúne o que a reversão pede, para comparar com a referência. É um
// struct de dados puro: a mesma regra vale para a entrada original e para a retomada.
type reversalIntent struct {
	kind     transaction.Kind
	playerID wallet.PlayerID
	walletID wallet.ID
	roundID  string
	amount   money.Money
}

func intentOf(parsed parsedOperation) reversalIntent {
	return reversalIntent{
		kind:     parsed.kind,
		playerID: parsed.playerID,
		walletID: parsed.walletID,
		roundID:  parsed.roundID,
		amount:   parsed.amount,
	}
}

// intentOfTransaction monta a intenção a partir de uma transação persistida (caminho
// do worker de referências).
func intentOfTransaction(tx *transaction.Transaction) reversalIntent {
	return reversalIntent{
		kind:     tx.Kind(),
		playerID: tx.PlayerID(),
		walletID: tx.WalletID(),
		roundID:  tx.RoundID(),
		amount:   tx.Amount(),
	}
}

// reversalConformanceFailure aplica as verificações do §7 e devolve o código de falha
// (string vazia = tudo confere). A ordem é parte do contrato: com mais de uma regra
// violada, o código é sempre o mesmo — rejeição reproduzível e auditável.
//
// O provedor não é comparado: a resolução já é por (providerId, externalId), então
// referência de outro provedor não seria encontrada — o que também evita revelar sua
// existência (§2).
func reversalConformanceFailure(intent reversalIntent, reference *transaction.Transaction) transaction.FailureCode {
	// 1. O tipo referenciado aceita esta reversão (REFUND->BET; ROLLBACK->BET/WIN/REFUND).
	if !reference.Kind().AllowsReferenceFrom(intent.kind) {
		return transaction.FailureReferenceKindNotAllowed
	}

	// 2. Jogador, carteira, rodada e moeda precisam concordar.
	if reference.PlayerID() != intent.playerID ||
		reference.WalletID() != intent.walletID ||
		reference.RoundID() != intent.roundID ||
		!reference.Amount().Currency().Equal(intent.amount.Currency()) {
		return transaction.FailureReferenceMismatch
	}

	// 3. Valor IDÊNTICO ao referenciado (reversões parciais fora de escopo, §7).
	if !reference.Amount().Equal(intent.amount) {
		return transaction.FailureAmountMismatch
	}

	return ""
}
