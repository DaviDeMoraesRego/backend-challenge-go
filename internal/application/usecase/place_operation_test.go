// Testes das regras aplicacionais de operações (sem banco): autorização (§2), política
// de valor por tipo (§7), idempotência/replay/conflito (§9), reversões (§7) e
// equivalência HTTP/SQS (§10). Não prova atomicidade nem concorrência entre processos
// — isso é coberto contra o Postgres real.
package usecase

import (
	"context"
	"errors"
	"testing"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// placeOK executa a operação exigindo sucesso de execução (rejeição de negócio é
// resultado válido, não erro).
func (h *harness) placeOK(t *testing.T, cmd ports.PlaceOperationCommand) ports.PlaceOperationResult {
	t.Helper()
	result, err := h.place.Execute(context.Background(), cmd)
	if err != nil {
		t.Fatalf("place.Execute unexpected error: %v", err)
	}
	return result
}

func TestPlaceOperation_BetDebitsWallet(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	result := h.placeOK(t, h.operation(t, w.WalletID, "BET", "25.00", "tx-1"))

	if result.State != transaction.StateProcessed {
		t.Fatalf("State = %q, want %q", result.State, transaction.StateProcessed)
	}
	if !result.BalanceKnown {
		t.Fatalf("BalanceKnown = false, want true em operação concluída")
	}
	if got := result.Balance.String(); got != "75.00" {
		t.Errorf("Balance = %q, want %q", got, "75.00")
	}
	if result.IdempotentReplay {
		t.Errorf("IdempotentReplay = true na primeira execução")
	}
	// Abertura (1) + aposta (1) = 2 lançamentos.
	if got := h.ledgerCount(t, w.WalletID); got != 2 {
		t.Errorf("lançamentos = %d, want 2", got)
	}
	// Dois eventos: operação concluída + saldo alterado.
	if got := len(h.outboxByType("WagerTransactionProcessed")); got != 2 {
		t.Errorf("WagerTransactionProcessed = %d, want 2 (abertura + aposta)", got)
	}
	if got := len(h.outboxByType("WalletBalanceChanged")); got != 2 {
		t.Errorf("WalletBalanceChanged = %d, want 2", got)
	}
}

// TestPlaceOperation_RaceScenario: cenário do §8 (100.00, duas apostas de 80.00) em
// nível aplicacional. Aqui são sequenciais; fixa o resultado exigido — uma processada,
// uma rejeitada por saldo, saldo 20.00 e um único débito no ledger.
func TestPlaceOperation_RaceScenario(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	first := h.placeOK(t, h.operation(t, w.WalletID, "BET", "80.00", "tx-1"))
	second := h.placeOK(t, h.operation(t, w.WalletID, "BET", "80.00", "tx-2"))

	if first.State != transaction.StateProcessed {
		t.Errorf("primeira aposta = %q, want %q", first.State, transaction.StateProcessed)
	}
	if second.State != transaction.StateRejected {
		t.Errorf("segunda aposta = %q, want %q", second.State, transaction.StateRejected)
	}
	if second.FailureCode != transaction.FailureBetInsufficientFunds {
		t.Errorf("FailureCode = %q, want %q", second.FailureCode, transaction.FailureBetInsufficientFunds)
	}
	if got := h.balanceOf(t, w.WalletID); got != "20.00" {
		t.Errorf("saldo final = %q, want %q", got, "20.00")
	}
	// Abertura + UMA aposta = 2 lançamentos: a rejeitada não gera ledger (§6.4).
	if got := h.ledgerCount(t, w.WalletID); got != 2 {
		t.Errorf("lançamentos = %d, want 2 (a rejeitada nao pode gerar ledger)", got)
	}
	// A rejeição é auditável e produz evento (§11).
	if got := len(h.outboxByType("WagerTransactionRejected")); got != 1 {
		t.Errorf("WagerTransactionRejected = %d, want 1", got)
	}
}

func TestPlaceOperation_Win(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	result := h.placeOK(t, h.operation(t, w.WalletID, "WIN", "50.00", "tx-1"))

	if result.State != transaction.StateProcessed {
		t.Fatalf("State = %q, want %q", result.State, transaction.StateProcessed)
	}
	if got := result.Balance.String(); got != "150.00" {
		t.Errorf("Balance = %q, want %q", got, "150.00")
	}
}

// TestPlaceOperation_Loss: LOSS não movimenta a carteira, mas conclui e produz
// WagerTransactionProcessed — sem ledger, sem WalletBalanceChanged e sem novo versionamento.
func TestPlaceOperation_Loss(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	ledgerBefore := h.ledgerCount(t, w.WalletID)
	balanceEventsBefore := len(h.outboxByType("WalletBalanceChanged"))

	result := h.placeOK(t, h.operation(t, w.WalletID, "LOSS", "0.00", "tx-1"))

	if result.State != transaction.StateProcessed {
		t.Fatalf("State = %q, want %q", result.State, transaction.StateProcessed)
	}
	if got := h.balanceOf(t, w.WalletID); got != "100.00" {
		t.Errorf("saldo = %q, want %q (LOSS nao movimenta)", got, "100.00")
	}
	if got := h.ledgerCount(t, w.WalletID); got != ledgerBefore {
		t.Errorf("lançamentos = %d, want %d (LOSS nao gera ledger)", got, ledgerBefore)
	}
	if got := len(h.outboxByType("WalletBalanceChanged")); got != balanceEventsBefore {
		t.Errorf("WalletBalanceChanged = %d, want %d (saldo nao mudou)", got, balanceEventsBefore)
	}
	if got := len(h.outboxByType("WagerTransactionProcessed")); got != 2 {
		t.Errorf("WagerTransactionProcessed = %d, want 2 (abertura + LOSS)", got)
	}
	// A versão da carteira não muda: nenhuma alteração de saldo (§6.2).
	if got := h.balanceOf(t, w.WalletID); got != "100.00" {
		t.Errorf("versao/saldo inesperados: %q", got)
	}
}

func TestPlaceOperation_ValuePolicy(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		amount string
	}{
		{name: "loss_com_valor_positivo", kind: "LOSS", amount: "25.00"},
		{name: "bet_com_valor_zero", kind: "BET", amount: "0.00"},
		{name: "win_com_valor_zero", kind: "WIN", amount: "0.00"},
		{name: "refund_com_valor_zero", kind: "REFUND", amount: "0.00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			w := h.seedWallet(t, "100.00")

			cmd := h.operation(t, w.WalletID, tc.kind, tc.amount, "tx-1")
			if tc.kind == "REFUND" {
				// REFUND exige referência para sequer chegar na política de valor.
				cmd.ReferenceExternalTransactionID = "tx-origem"
			}

			if _, err := h.place.Execute(context.Background(), cmd); !errors.Is(err, ports.ErrValidation) {
				t.Errorf("error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestPlaceOperation_RejectsOpeningFromExternalChannel(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	// §6.3: OPENING é interno e deve ser REJEITADO quando vem por HTTP/SQS.
	_, err := h.place.Execute(context.Background(), h.operation(t, w.WalletID, "OPENING", "25.00", "tx-1"))
	if !errors.Is(err, ports.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestPlaceOperation_RejectsUnknownKind(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	_, err := h.place.Execute(context.Background(), h.operation(t, w.WalletID, "CASHOUT", "25.00", "tx-1"))
	if !errors.Is(err, ports.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestPlaceOperation_Authorization(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	t.Run("provedor_nao_opera_em_nome_de_outro", func(t *testing.T) {
		cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
		// O token diz provider-b, o payload diz provider-a.
		cmd.Principal = providerPrincipal(testProviderB)

		_, err := h.place.Execute(context.Background(), cmd)
		if !errors.Is(err, ports.ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
		if got := h.balanceOf(t, w.WalletID); got != "100.00" {
			t.Errorf("saldo = %q: acesso negado NAO pode ter efeito financeiro", got)
		}
	})

	t.Run("servico_interno_autorizado", func(t *testing.T) {
		cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-2")
		// O SQS usa identidade INTERNA: o controle de acesso é do broker (§2).
		cmd.Principal = internalPrincipal()

		if _, err := h.place.Execute(context.Background(), cmd); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("escopo_desconhecido", func(t *testing.T) {
		cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-3")
		cmd.Principal = ports.Principal{Scope: ports.Scope("admin")}

		_, err := h.place.Execute(context.Background(), cmd)
		if !errors.Is(err, ports.ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	})
}

func TestPlaceOperation_IdempotentReplay(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")

	first := h.placeOK(t, cmd)
	ledgerAfterFirst := h.ledgerCount(t, w.WalletID)
	eventsAfterFirst := len(h.outboxByType("WalletBalanceChanged"))

	second := h.placeOK(t, cmd)

	if !second.IdempotentReplay {
		t.Errorf("IdempotentReplay = false, want true no reenvio")
	}
	if first.TransactionID != second.TransactionID {
		t.Errorf("TransactionID diferente no replay: %q != %q", first.TransactionID, second.TransactionID)
	}
	if got := h.balanceOf(t, w.WalletID); got != "75.00" {
		t.Errorf("saldo = %q, want %q (o replay NAO pode debitar de novo)", got, "75.00")
	}
	if got := h.ledgerCount(t, w.WalletID); got != ledgerAfterFirst {
		t.Errorf("lançamentos = %d, want %d (replay nao gera ledger)", got, ledgerAfterFirst)
	}
	if got := len(h.outboxByType("WalletBalanceChanged")); got != eventsAfterFirst {
		t.Errorf("eventos de saldo = %d, want %d (replay nao gera evento)", got, eventsAfterFirst)
	}
}

// TestPlaceOperation_ReplayReturnsOriginalBalance: o replay devolve o saldo do
// processamento original, mesmo com movimentações posteriores (§9).
func TestPlaceOperation_ReplayReturnsOriginalBalance(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	bet := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
	first := h.placeOK(t, bet) // saldo observado: 75.00

	// Outra movimentação muda o saldo para 85.00.
	h.placeOK(t, h.operation(t, w.WalletID, "WIN", "10.00", "tx-2"))

	// O replay da PRIMEIRA operação deve devolver 75.00 (o original), e não 85.00.
	replay := h.placeOK(t, bet)

	if !replay.IdempotentReplay {
		t.Fatalf("IdempotentReplay = false, want true")
	}
	if got := replay.Balance.String(); got != first.Balance.String() {
		t.Errorf("saldo do replay = %q, want %q (saldo do processamento ORIGINAL)", got, first.Balance.String())
	}
	if got := replay.Balance.String(); got != "75.00" {
		t.Errorf("saldo do replay = %q, want %q", got, "75.00")
	}
}

func TestPlaceOperation_ConflictSameKeyDifferentPayload(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	h.placeOK(t, h.operation(t, w.WalletID, "BET", "25.00", "tx-1"))

	// MESMA chave de idempotência, VALOR diferente -> conflito (§9).
	changed := h.operation(t, w.WalletID, "BET", "25.01", "tx-1")

	_, err := h.place.Execute(context.Background(), changed)
	if !errors.Is(err, ports.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
	// Nada foi aplicado.
	if got := h.balanceOf(t, w.WalletID); got != "75.00" {
		t.Errorf("saldo = %q, want %q", got, "75.00")
	}
}

func TestPlaceOperation_ConflictSameExternalIDDifferentKey(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	h.placeOK(t, h.operation(t, w.WalletID, "BET", "25.00", "tx-1"))

	// §9: a MESMA operação (providerId, externalId) reenviada com OUTRA chave de
	// idempotência não pode ser reaplicada.
	otherKey := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
	otherKey.IdempotencyKey = "provider-a:uma-chave-nova"

	_, err := h.place.Execute(context.Background(), otherKey)
	if !errors.Is(err, ports.ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
	if got := h.balanceOf(t, w.WalletID); got != "75.00" {
		t.Errorf("saldo = %q, want %q", got, "75.00")
	}
}

// TestPlaceOperation_HttpAndSqsProduceSameIdentity: a mesma operação por HTTP e por SQS
// é reconhecida como a mesma (hash canônico idêntico nos dois caminhos, §9/§10).
func TestPlaceOperation_HttpAndSqsProduceSameIdentity(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	// 1) Chegou por HTTP (sem inbox).
	httpCmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
	first := h.placeOK(t, httpCmd)

	// 2) A MESMA operação chega por SQS (com inbox e outro correlationId).
	sqsCmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
	sqsCmd.InboxConsumer = "wager-transactions-worker"
	sqsCmd.InboxMessageID = "msg-123"
	sqsCmd.InboxMessageHash = "hash-do-corpo"
	sqsCmd.CorrelationID = "corr-do-sqs"

	second := h.placeOK(t, sqsCmd)

	if !second.IdempotentReplay {
		t.Fatalf("IdempotentReplay = false: o SQS nao reconheceu a operacao do HTTP")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("TransactionID = %q, want %q", second.TransactionID, first.TransactionID)
	}
	if got := h.balanceOf(t, w.WalletID); got != "75.00" {
		t.Errorf("saldo = %q, want %q (nao pode debitar duas vezes)", got, "75.00")
	}
}

func TestPlaceOperation_InboxRejectsRedeliveryWithDifferentContent(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
	cmd.InboxConsumer = "worker"
	cmd.InboxMessageID = "msg-123"
	cmd.InboxMessageHash = "hash-original"

	h.placeOK(t, cmd)

	// Mesmo `messageId` com OUTRO conteúdo: mensagem inválida (§10) — rejeição
	// permanente, que o adapter deve levar à DLQ, nunca aplicar de novo.
	tampered := cmd
	tampered.IdempotencyKey = "provider-a:outra-chave"
	tampered.ExternalTransactionID = "tx-2"
	tampered.InboxMessageHash = "hash-diferente"

	_, err := h.place.Execute(context.Background(), tampered)
	if !errors.Is(err, ports.ErrValidation) {
		t.Errorf("error = %v, want ErrValidation", err)
	}
}

func TestPlaceOperation_WalletGuards(t *testing.T) {
	t.Run("carteira_inexistente", func(t *testing.T) {
		h := newHarness(t)
		h.seedWallet(t, "100.00")
		before := len(h.store.transactions) // já contém o OPENING da abertura

		// Carteira inexistente é 404, não rejeição persistida (wallet_id tem FK).
		cmd := h.operation(t, wallet.ID("id-inexistente"), "BET", "25.00", "tx-1")
		_, err := h.place.Execute(context.Background(), cmd)
		if !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("error = %v, want ErrNotFound", err)
		}
		if len(h.store.transactions) != before {
			t.Errorf("transacoes persistidas = %d, want %d (nada novo)", len(h.store.transactions), before)
		}
	})

	t.Run("jogador_diferente_da_carteira", func(t *testing.T) {
		h := newHarness(t)
		w := h.seedWallet(t, "100.00")

		cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
		cmd.PlayerID = "0192f28f-0000-7000-8000-000000000099"

		result := h.placeOK(t, cmd)
		if result.FailureCode != transaction.FailureWalletNotFound {
			t.Errorf("FailureCode = %q, want %q (nao vazamos que a carteira existe para outro jogador)",
				result.FailureCode, transaction.FailureWalletNotFound)
		}
	})

	t.Run("moeda_incompativel", func(t *testing.T) {
		h := newHarness(t)
		w := h.seedWallet(t, "100.00")

		cmd := h.operation(t, w.WalletID, "BET", "25.00", "tx-1")
		cmd.Currency = "USD"

		result := h.placeOK(t, cmd)
		if result.FailureCode != transaction.FailureCurrencyMismatch {
			t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureCurrencyMismatch)
		}
	})
}

// bet executa uma BET e exige que ela seja processada.
func (h *harness) bet(t *testing.T, w ports.OpenWalletResult, amount, externalID string) {
	t.Helper()
	result := h.placeOK(t, h.operation(t, w.WalletID, "BET", amount, externalID))
	if result.State != transaction.StateProcessed {
		t.Fatalf("BET %s deveria ser processada, got %q (%s)", externalID, result.State, result.FailureCode)
	}
}

func TestPlaceOperation_RefundReturnsBet(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")
	h.bet(t, w, "25.00", "bet-1")

	refund := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-1")
	refund.ReferenceExternalTransactionID = "bet-1"

	result := h.placeOK(t, refund)

	if result.State != transaction.StateProcessed {
		t.Fatalf("State = %q, want %q (%s)", result.State, transaction.StateProcessed, result.FailureCode)
	}
	if got := result.Balance.String(); got != "100.00" {
		t.Errorf("saldo = %q, want %q (devolucao integral)", got, "100.00")
	}
}

func TestPlaceOperation_RollbackUndoesBet(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")
	h.bet(t, w, "25.00", "bet-1")

	rollback := h.operation(t, w.WalletID, "ROLLBACK", "25.00", "rollback-1")
	rollback.ReferenceExternalTransactionID = "bet-1"

	result := h.placeOK(t, rollback)

	if result.State != transaction.StateProcessed {
		t.Fatalf("State = %q, want %q (%s)", result.State, transaction.StateProcessed, result.FailureCode)
	}
	// Desfazer um DÉBITO é um CRÉDITO: 75.00 + 25.00 = 100.00.
	if got := result.Balance.String(); got != "100.00" {
		t.Errorf("saldo = %q, want %q", got, "100.00")
	}
}

// TestPlaceOperation_RollbackOfWinNeedsBalance: a reversão que não consegue debitar usa
// código próprio, diferente de "aposta sem saldo" (§7).
func TestPlaceOperation_RollbackOfWinNeedsBalance(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	// WIN de 150 (saldo 250) e depois aposta de 240 (saldo 10).
	h.placeOK(t, h.operation(t, w.WalletID, "WIN", "150.00", "win-1"))
	h.bet(t, w, "240.00", "bet-1")
	if got := h.balanceOf(t, w.WalletID); got != "10.00" {
		t.Fatalf("saldo preparado = %q, want %q", got, "10.00")
	}

	// Desfazer o WIN exigiria debitar 150 com apenas 10 disponíveis.
	rollback := h.operation(t, w.WalletID, "ROLLBACK", "150.00", "rollback-1")
	rollback.ReferenceExternalTransactionID = "win-1"

	result := h.placeOK(t, rollback)

	if result.State != transaction.StateRejected {
		t.Fatalf("State = %q, want %q", result.State, transaction.StateRejected)
	}
	// §7: código DIFERENTE do usado em "aposta sem saldo".
	if result.FailureCode != transaction.FailureReversalInsufficientFunds {
		t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureReversalInsufficientFunds)
	}
	if result.FailureCode == transaction.FailureBetInsufficientFunds {
		t.Errorf("o codigo de reversao NAO pode ser igual ao de aposta sem saldo (§7)")
	}
}

func TestPlaceOperation_ReversalValidation(t *testing.T) {
	t.Run("refund_de_win_nao_e_permitido", func(t *testing.T) {
		h := newHarness(t)
		w := h.seedWallet(t, "100.00")
		h.placeOK(t, h.operation(t, w.WalletID, "WIN", "50.00", "win-1"))

		refund := h.operation(t, w.WalletID, "REFUND", "50.00", "refund-1")
		refund.ReferenceExternalTransactionID = "win-1"

		result := h.placeOK(t, refund)
		if result.FailureCode != transaction.FailureReferenceKindNotAllowed {
			t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureReferenceKindNotAllowed)
		}
	})

	t.Run("valor_diferente_do_referenciado", func(t *testing.T) {
		h := newHarness(t)
		w := h.seedWallet(t, "100.00")
		h.bet(t, w, "25.00", "bet-1")

		refund := h.operation(t, w.WalletID, "REFUND", "10.00", "refund-1")
		refund.ReferenceExternalTransactionID = "bet-1"

		result := h.placeOK(t, refund)
		if result.FailureCode != transaction.FailureAmountMismatch {
			t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureAmountMismatch)
		}
	})

	t.Run("duas_reversoes_da_mesma_referencia", func(t *testing.T) {
		h := newHarness(t)
		w := h.seedWallet(t, "100.00")
		h.bet(t, w, "25.00", "bet-1")

		refund := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-1")
		refund.ReferenceExternalTransactionID = "bet-1"
		h.placeOK(t, refund)

		// §7: "Garanta que uma referência não receba duas reversões bem-sucedidas."
		second := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-2")
		second.ReferenceExternalTransactionID = "bet-1"

		result := h.placeOK(t, second)
		if result.FailureCode != transaction.FailureDuplicateReversal {
			t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureDuplicateReversal)
		}
		// O saldo não pode ter sido devolvido duas vezes.
		if got := h.balanceOf(t, w.WalletID); got != "100.00" {
			t.Errorf("saldo = %q, want %q", got, "100.00")
		}
	})
}

// TestPlaceOperation_ReversalBeforeReference prova o cenário do §3: a reversão
// chega ANTES da transação que ela referencia.
func TestPlaceOperation_ReversalBeforeReference(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	refund := h.operation(t, w.WalletID, "REFUND", "25.00", "refund-1")
	refund.ReferenceExternalTransactionID = "bet-que-nao-chegou"

	result := h.placeOK(t, refund)

	if result.State != transaction.StatePendingReference {
		t.Fatalf("State = %q, want %q", result.State, transaction.StatePendingReference)
	}
	if result.FailureCode != transaction.FailureReferenceNotFound {
		t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureReferenceNotFound)
	}
	// §7: a pendência é PERSISTIDA e ganha evento próprio (§11).
	if got := len(h.outboxByType("WagerTransactionPendingReference")); got != 1 {
		t.Errorf("WagerTransactionPendingReference = %d, want 1", got)
	}
	// Nada foi movimentado.
	if got := h.balanceOf(t, w.WalletID); got != "100.00" {
		t.Errorf("saldo = %q, want %q", got, "100.00")
	}
}

// TestPlaceOperation_ReferenceRejectedIsDefinitive prova que uma referência que
// terminou SEM sucesso leva a rejeição imediata — esperar seria esperar para
// sempre.
func TestPlaceOperation_ReferenceRejectedIsDefinitive(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	// BET que será rejeitada por saldo insuficiente.
	rejected := h.placeOK(t, h.operation(t, w.WalletID, "BET", "999.00", "bet-1"))
	if rejected.State != transaction.StateRejected {
		t.Fatalf("preparacao invalida: BET deveria ser rejeitada")
	}

	refund := h.operation(t, w.WalletID, "REFUND", "999.00", "refund-1")
	refund.ReferenceExternalTransactionID = "bet-1"

	result := h.placeOK(t, refund)
	if result.FailureCode != transaction.FailureReferenceNotProcessed {
		t.Errorf("FailureCode = %q, want %q", result.FailureCode, transaction.FailureReferenceNotProcessed)
	}
}

// TestPlaceOperation_OutboxEnvelope: envelope no topo, `data` tipado e valores
// monetários como string decimal (§11).
func TestPlaceOperation_OutboxEnvelope(t *testing.T) {
	h := newHarness(t)
	w := h.seedWallet(t, "100.00")

	h.placeOK(t, h.operation(t, w.WalletID, "BET", "25.00", "tx-1"))

	balanceEvents := h.outboxByType("WalletBalanceChanged")
	if len(balanceEvents) != 2 {
		t.Fatalf("WalletBalanceChanged = %d, want 2", len(balanceEvents))
	}
	betEvent := balanceEvents[1] // o segundo é o da aposta

	payloadContains(t, betEvent, `"eventType":"WalletBalanceChanged"`)
	payloadContains(t, betEvent, `"direction":"DEBIT"`)
	payloadContains(t, betEvent, `"amount":"25.00"`)
	// Saldos são OBJETOS Money (valor + moeda), não strings soltas — é assim que
	// o §11 define o payload, e é o que garante que a moeda viaje junto.
	payloadContains(t, betEvent, `"balanceBefore":{"amount":"100.00","currency":"BRL"}`)
	payloadContains(t, betEvent, `"balanceAfter":{"amount":"75.00","currency":"BRL"}`)
	payloadContains(t, betEvent, `"walletVersion":2`)
	// aggregateId do evento de saldo é a CARTEIRA, não a transação.
	if betEvent.AggregateID != w.WalletID.String() {
		t.Errorf("AggregateID = %q, want %q", betEvent.AggregateID, w.WalletID.String())
	}
	if betEvent.AggregateType != "Wallet" {
		t.Errorf("AggregateType = %q, want %q", betEvent.AggregateType, "Wallet")
	}
}
