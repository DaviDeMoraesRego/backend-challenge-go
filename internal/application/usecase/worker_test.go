// Testes dos casos de uso de processo: worker de referências (§7: retomada, backoff,
// expiração) e relay do outbox (§11: publicação, confirmação, backoff e preservação do
// eventId em republicações).
package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
)

// TestResolvePendingReferences_ResolvesWhenReferenceArrives: a reversão chega antes (§3),
// fica pendente, o worker reagenda; quando a referência chega, a retomada conclui.
func TestResolvePendingReferences_ResolvesWhenReferenceArrives(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	// A REFUND chega antes da BET que ela referencia.
	refund := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-1")
	refund.ReferenceExternalTransactionID = "bet-1"

	pending := h.placeOK(t, refund)
	if pending.State != transaction.StatePendingReference {
		t.Fatalf("State = %q, want %q", pending.State, transaction.StatePendingReference)
	}

	// --- Rodada 1: ainda antes do agendamento vencer -> NADA é varrido --------
	early, err := h.resolver.Execute(context.Background())
	if err != nil {
		t.Fatalf("resolver.Execute unexpected error: %v", err)
	}
	if early.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0 (agendamento ainda nao venceu)", early.Scanned)
	}

	// --- Rodada 2: venceu, mas a referência ainda não existe -> reagenda ------
	h.clock.advance(time.Second)
	run2, err := h.resolver.Execute(context.Background())
	if err != nil {
		t.Fatalf("resolver.Execute unexpected error: %v", err)
	}
	if run2.Rescheduled != 1 {
		t.Fatalf("Rescheduled = %d, want 1 (referencia ainda ausente)", run2.Rescheduled)
	}

	// --- A BET referenciada chega (pelo caminho normal) ----------------------
	h.bet(t, w, "25.00", "bet-1")
	if got := h.balanceOf(t, w.WalletID); got != "75.00" {
		t.Fatalf("saldo apos a BET = %q, want %q", got, "75.00")
	}

	// --- Rodada 3: a referência existe e está PROCESSED -> CONCLUI -----------
	h.clock.advance(time.Hour)
	run3, err := h.resolver.Execute(context.Background())
	if err != nil {
		t.Fatalf("resolver.Execute unexpected error: %v", err)
	}
	if run3.Resolved != 1 {
		t.Fatalf("Resolved = %d, want 1", run3.Resolved)
	}

	// A devolução foi aplicada: 75.00 + 25.00.
	if got := h.balanceOf(t, w.WalletID); got != "100.00" {
		t.Errorf("saldo final = %q, want %q", got, "100.00")
	}
	// Abertura + BET + REFUND = 3 lançamentos.
	if got := h.ledgerCount(t, w.WalletID); got != 3 {
		t.Errorf("lançamentos = %d, want 3", got)
	}
	// O evento de conclusão da REFUND foi emitido na retomada (§11).
	if got := len(h.outboxByType("WagerTransactionProcessed")); got != 3 {
		t.Errorf("WagerTransactionProcessed = %d, want 3 (abertura, BET e REFUND)", got)
	}
}

// TestResolvePendingReferences_ExpiresAfterMaxAttempts: esgotado o teto de tentativas,
// finaliza como REJECTED com REFERENCE_NOT_FOUND (§7).
func TestResolvePendingReferences_ExpiresAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	refund := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-1")
	refund.ReferenceExternalTransactionID = "bet-que-nunca-chega"
	h.placeOK(t, refund)

	// O harness configura MaxAttempts = 3; cada rodada avança o relógio para vencer o
	// backoff. Acumulamos os resultados: depois que a pendência é rejeitada ela sai da
	// varredura e as rodadas seguintes voltam com zero — olhar só a última tornaria o
	// teste verde por engano.
	totalRejected := 0
	for i := 0; i < 4; i++ {
		h.clock.advance(time.Hour)
		result, err := h.resolver.Execute(context.Background())
		if err != nil {
			t.Fatalf("resolver.Execute unexpected error: %v", err)
		}
		totalRejected += result.Rejected
	}

	if totalRejected != 1 {
		t.Fatalf("Rejected = %d, want 1 (tentativas esgotadas)", totalRejected)
	}

	// O estado final é REJECTED com o código de referência não encontrada.
	tx, err := (&fakeTransactionRepository{store: h.store}).FindByProviderAndExternalID(
		context.Background(), testProviderA, "refund-1")
	if err != nil {
		t.Fatalf("FindByProviderAndExternalID unexpected error: %v", err)
	}
	if tx.State() != transaction.StateRejected {
		t.Errorf("State = %q, want %q", tx.State(), transaction.StateRejected)
	}
	if tx.FailureCode() != transaction.FailureReferenceNotFound {
		t.Errorf("FailureCode = %q, want %q", tx.FailureCode(), transaction.FailureReferenceNotFound)
	}
	// O §7 exige que a rejeição produza EVENTO.
	if got := len(h.outboxByType("WagerTransactionRejected")); got != 1 {
		t.Errorf("WagerTransactionRejected = %d, want 1", got)
	}
	// Nada foi movimentado: a devolução nunca ocorreu.
	if got := h.balanceOf(t, w.WalletID); got != "100.00" {
		t.Errorf("saldo = %q, want %q", got, "100.00")
	}
}

// TestResolvePendingReferences_BackoffGrows: o intervalo entre tentativas aumenta
// (backoff exponencial), em vez de ficar constante.
func TestResolvePendingReferences_BackoffGrows(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	refund := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-1")
	refund.ReferenceExternalTransactionID = "bet-1"
	h.placeOK(t, refund)

	repository := &fakeTransactionRepository{store: h.store}

	// Primeira retomada: attempts 1 -> 2, atraso base (1s).
	h.clock.advance(time.Second)
	if _, err := h.resolver.Execute(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tx, err := repository.FindByProviderAndExternalID(context.Background(), testProviderA, "refund-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	firstDelay := tx.NextRetryAt().Sub(h.clock.Now())

	// Segunda retomada: attempts 2 -> 3, atraso dobrado (2s).
	h.clock.advance(firstDelay)
	if _, err := h.resolver.Execute(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tx, err = repository.FindByProviderAndExternalID(context.Background(), testProviderA, "refund-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	secondDelay := tx.NextRetryAt().Sub(h.clock.Now())

	if secondDelay <= firstDelay {
		t.Errorf("atraso nao cresceu: primeiro=%v segundo=%v", firstDelay, secondDelay)
	}
	if secondDelay != 2*firstDelay {
		t.Errorf("atraso = %v, want o dobro de %v (backoff exponencial)", secondDelay, firstDelay)
	}
}

func TestRelayOutbox_PublishesAndConfirms(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")
	h.placeOK(t, h.operation(t, w.WalletID, "BET", "25.00", "tx-1"))

	// Abertura (2 eventos) + aposta (2 eventos) = 4 pendentes.
	total := len(h.store.outbox)
	if total != 4 {
		t.Fatalf("eventos na outbox = %d, want 4", total)
	}

	result, err := h.relay.Execute(context.Background())
	if err != nil {
		t.Fatalf("relay.Execute unexpected error: %v", err)
	}

	if result.Claimed != total || result.Published != total {
		t.Errorf("Claimed/Published = %d/%d, want %d/%d", result.Claimed, result.Published, total, total)
	}
	if got := h.publisher.count(); got != total {
		t.Errorf("publicados no broker = %d, want %d", got, total)
	}

	// Segunda rodada: nada pendente (publicação é idempotente por estado).
	second, err := h.relay.Execute(context.Background())
	if err != nil {
		t.Fatalf("segunda rodada unexpected error: %v", err)
	}
	if second.Claimed != 0 {
		t.Errorf("Claimed na segunda rodada = %d, want 0", second.Claimed)
	}
}

// TestRelayOutbox_ReschedulesAndPreservesEventID: retry com backoff e republicação que
// preserva o eventId (§11).
func TestRelayOutbox_ReschedulesAndPreservesEventID(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")
	h.placeOK(t, h.operation(t, w.WalletID, "BET", "25.00", "tx-1"))

	// Falha a PRIMEIRA publicação (a ordem dos eventos vem da ordem de inserção).
	h.publisher.failNext = errors.New("broker indisponivel")

	result, err := h.relay.Execute(context.Background())
	if err != nil {
		t.Fatalf("relay.Execute unexpected error: %v", err)
	}
	if result.Published != len(h.store.outbox)-1 {
		t.Errorf("Published = %d, want %d", result.Published, len(h.store.outbox)-1)
	}
	if result.Rescheduled != 1 {
		t.Fatalf("Rescheduled = %d, want 1", result.Rescheduled)
	}

	// Identifica o evento reagendado.
	var rescheduledID string
	for _, record := range h.store.outbox {
		if record.Attempts > 0 {
			rescheduledID = record.EventID
		}
	}
	if rescheduledID == "" {
		t.Fatalf("nenhum evento foi reagendado")
	}

	// Avança o relógio para vencer o backoff e roda de novo.
	h.clock.advance(time.Hour)
	if _, err := h.relay.Execute(context.Background()); err != nil {
		t.Fatalf("segunda rodada unexpected error: %v", err)
	}

	// O MESMO eventId foi republicado (§11).
	found := false
	for _, published := range h.publisher.published {
		if published.EventID == rescheduledID {
			found = true
		}
	}
	if !found {
		t.Errorf("eventId %q nao foi republicado — republicacao DEVE preservar o eventId", rescheduledID)
	}
}

// TestRelayOutbox_GivesUpAfterMaxAttempts: no teto o evento para de ser tentado, mas não
// é apagado (permanece auditável).
func TestRelayOutbox_GivesUpAfterMaxAttempts(t *testing.T) {
	h := newHarness(t)
	h.seedWallet(t, "100.00") // 2 eventos de abertura

	// MaxAttempts = 3 no harness: falha três rodadas seguidas.
	for i := 0; i < 3; i++ {
		h.publisher.failNext = errors.New("broker fora")
		if _, err := h.relay.Execute(context.Background()); err != nil {
			t.Fatalf("rodada %d unexpected error: %v", i, err)
		}
		// Empurra o evento de volta para "vencido" a cada rodada, simulando a
		// passagem do tempo do backoff.
		h.clock.advance(48 * time.Hour)
	}

	// Um dos eventos atingiu o teto.
	var exhausted bool
	for _, record := range h.store.outbox {
		if record.Attempts >= 3 && record.PublishedAt.IsZero() {
			exhausted = true
		}
	}
	if !exhausted {
		t.Errorf("nenhum evento atingiu o teto de tentativas")
	}
	// A tabela continua com TODOS os eventos: nada é descartado (§11 pede
	// retry com backoff, e o descarte deixaria o incidente invisível).
	if len(h.store.outbox) != 2 {
		t.Errorf("eventos na outbox = %d, want 2 (nada pode ser apagado)", len(h.store.outbox))
	}

	// O evento desistido não é mais reivindicado, mesmo após o relógio avançar.
	h.clock.advance(72 * time.Hour)
	after, err := h.relay.Execute(context.Background())
	if err != nil {
		t.Fatalf("rodada apos o teto unexpected error: %v", err)
	}
	if after.Claimed != 0 {
		t.Errorf("Claimed apos o teto = %d, want 0 (dead-letter nao pode ser republicado)", after.Claimed)
	}
}
