// Prova que os eventos de integração (§11) honram o contrato: TIPO e VERSÃO fixados
// pelo construtor, header do envelope no topo do JSON (embed) com o payload em `data`,
// validações estruturais e teste golden do formato de WalletBalanceChanged.
package event

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var eventTime = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

const (
	testWalletID      wallet.ID                         = "0192f291-27dd-7d3f-8071-5f8685deef37"
	testTransactionID transaction.ID                    = "0192f298-345e-7e38-af88-e43f851a819d"
	testPlayerID      wallet.PlayerID                   = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	testProviderID    transaction.ProviderID            = "provider-a"
	testExternalID    transaction.ExternalTransactionID = "transaction-123"
)

func mustMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q) unexpected error: %v", amount, err)
	}
	return m
}

// mustTransaction cria uma transação externa válida para servir de origem aos eventos.
func mustTransaction(t *testing.T, kind transaction.Kind, amount string) *transaction.Transaction {
	t.Helper()

	hash, err := transaction.NewPayloadHash(strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("NewPayloadHash unexpected error: %v", err)
	}

	params := transaction.NewExternalParams{
		ID:                    testTransactionID,
		Kind:                  kind,
		PlayerID:              testPlayerID,
		WalletID:              testWalletID,
		Amount:                mustMoney(t, amount, money.BRL),
		ProviderID:            testProviderID,
		ExternalTransactionID: testExternalID,
		IdempotencyKey:        transaction.IdempotencyKey("provider-a:transaction-123"),
		PayloadHash:           hash,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Now:                   eventTime,
	}
	if kind.IsReversal() {
		params.ReferenceExternalTransactionID = transaction.ExternalTransactionID("transaction-000")
	}

	tx, err := transaction.NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal unexpected error: %v", err)
	}
	return tx
}

// mustMovement gera um movimento REAL de carteira (crédito de abertura).
func mustMovement(t *testing.T, amount string) wallet.Movement {
	t.Helper()

	id, err := wallet.NewID(string(testWalletID))
	if err != nil {
		t.Fatalf("wallet.NewID unexpected error: %v", err)
	}
	w, err := wallet.New(id, testPlayerID, mustMoney(t, "0.00", money.BRL), eventTime)
	if err != nil {
		t.Fatalf("wallet.New unexpected error: %v", err)
	}
	movement, err := w.Credit(mustMoney(t, amount, money.BRL), eventTime)
	if err != nil {
		t.Fatalf("Credit unexpected error: %v", err)
	}
	return movement
}

// TestEnvelope_Validation cobre as validações estruturais do header (§11).
func TestEnvelope_Validation(t *testing.T) {
	valid := envelopeParams{
		EventID:       "evt-1",
		EventType:     TypeWagerTransactionProcessed,
		AggregateID:   "agg-1",
		CorrelationID: "corr-1",
		OccurredAt:    eventTime,
	}

	cases := []struct {
		name   string
		mutate func(p *envelopeParams)
		want   error
	}{
		{name: "sem_tipo", mutate: func(p *envelopeParams) { p.EventType = "" }, want: ErrMissingEventType},
		{name: "tipo_desconhecido", mutate: func(p *envelopeParams) { p.EventType = "FooEvent" }, want: ErrUnknownEventType},
		{name: "sem_event_id", mutate: func(p *envelopeParams) { p.EventID = "" }, want: ErrMissingEventID},
		{name: "sem_aggregate_id", mutate: func(p *envelopeParams) { p.AggregateID = "" }, want: ErrMissingAggregateID},
		{name: "sem_correlation_id", mutate: func(p *envelopeParams) { p.CorrelationID = "" }, want: ErrMissingCorrelationID},
		{name: "sem_timestamp", mutate: func(p *envelopeParams) { p.OccurredAt = time.Time{} }, want: ErrInvalidTimestamp},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := valid
			tc.mutate(&params)

			_, err := newEnvelope(params)
			if err == nil {
				t.Fatalf("newEnvelope expected error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want sentinel %v", err, tc.want)
			}
		})
	}

	t.Run("valido_define_versao", func(t *testing.T) {
		env, err := newEnvelope(valid)
		if err != nil {
			t.Fatalf("newEnvelope unexpected error: %v", err)
		}
		// §11: a versão é definida pelo construtor.
		if env.Version != VersionV1 {
			t.Errorf("Version = %d, want %d", env.Version, VersionV1)
		}
		if !env.OccurredAt.Equal(eventTime) {
			t.Errorf("OccurredAt = %v, want %v", env.OccurredAt, eventTime)
		}
	})
}

// TestNewWagerTransactionProcessed cobre o evento de conclusão (inclusive LOSS).
func TestNewWagerTransactionProcessed(t *testing.T) {
	t.Run("bet_concluida", func(t *testing.T) {
		tx := mustTransaction(t, transaction.KindBet, "25.00")
		resultBalance := mustMoney(t, "975.00", money.BRL)

		event, err := NewWagerTransactionProcessed("evt-1", "corr-1", "", tx, resultBalance, eventTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Tipo fixado pelo construtor (§11).
		if event.EventType != TypeWagerTransactionProcessed {
			t.Errorf("EventType = %q, want %q", event.EventType, TypeWagerTransactionProcessed)
		}
		if event.Version != VersionV1 {
			t.Errorf("Version = %d, want %d", event.Version, VersionV1)
		}
		// aggregateId aponta para a TRANSAÇÃO, que é o agregado deste evento.
		if event.AggregateID != tx.ID().String() {
			t.Errorf("AggregateID = %q, want %q", event.AggregateID, tx.ID().String())
		}
		if event.Data.Kind != string(transaction.KindBet) {
			t.Errorf("Data.Kind = %q, want %q", event.Data.Kind, transaction.KindBet)
		}
		if event.Data.ResultBalance.String() != "975.00" {
			t.Errorf("Data.ResultBalance = %q, want %q", event.Data.ResultBalance.String(), "975.00")
		}
	})

	t.Run("loss_tambem_produz_evento", func(t *testing.T) {
		// §7/§11: LOSS não movimenta a carteira, mas PRODUZ WagerTransactionProcessed.
		tx := mustTransaction(t, transaction.KindLoss, "0.00")
		unchanged := mustMoney(t, "100.00", money.BRL)

		event, err := NewWagerTransactionProcessed("evt-2", "corr-1", "", tx, unchanged, eventTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.Data.Kind != string(transaction.KindLoss) {
			t.Errorf("Data.Kind = %q, want %q", event.Data.Kind, transaction.KindLoss)
		}
		// O saldo registrado é o saldo no momento (inalterado).
		if event.Data.ResultBalance.String() != "100.00" {
			t.Errorf("Data.ResultBalance = %q, want %q", event.Data.ResultBalance.String(), "100.00")
		}
	})

	t.Run("rejeita_saldo_sem_moeda", func(t *testing.T) {
		tx := mustTransaction(t, transaction.KindBet, "25.00")

		_, err := NewWagerTransactionProcessed("evt-3", "corr-1", "", tx, money.Money{}, eventTime)
		if !errors.Is(err, ErrMissingMoney) {
			t.Errorf("error = %v, want ErrMissingMoney", err)
		}
	})
}

// TestNewWalletBalanceChanged valida o evento (§11) e trava o formato JSON via golden.
func TestNewWalletBalanceChanged(t *testing.T) {
	movement := mustMovement(t, "25.00")

	event, err := NewWalletBalanceChanged("evt-1", "corr-1", "", testWalletID, testTransactionID, movement, eventTime)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// aggregateId é a CARTEIRA: é ela o agregado deste evento.
	if event.AggregateID != string(testWalletID) {
		t.Errorf("AggregateID = %q, want %q", event.AggregateID, testWalletID)
	}
	if event.Data.Direction != wallet.Credit {
		t.Errorf("Data.Direction = %q, want %q", event.Data.Direction, wallet.Credit)
	}
	if event.Data.WalletVersion != movement.WalletVersion() {
		t.Errorf("Data.WalletVersion = %d, want %d", event.Data.WalletVersion, movement.WalletVersion())
	}

	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal unexpected error: %v", err)
	}

	// GOLDEN: campos do Envelope no topo (embed), payload em `data`; `causationId`
	// some por ser omitempty.
	const want = `{"eventId":"evt-1","eventType":"WalletBalanceChanged","aggregateId":"0192f291-27dd-7d3f-8071-5f8685deef37","correlationId":"corr-1","occurredAt":"2026-09-08T12:00:00Z","version":1,"data":{"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37","transactionId":"0192f298-345e-7e38-af88-e43f851a819d","direction":"CREDIT","money":{"amount":"25.00","currency":"BRL"},"balanceBefore":{"amount":"0.00","currency":"BRL"},"balanceAfter":{"amount":"25.00","currency":"BRL"},"walletVersion":2}}`

	if string(encoded) != want {
		t.Errorf("JSON golden divergiu.\n got: %s\nwant: %s", encoded, want)
	}
}

// TestNewWagerTransactionRejected valida o evento de rejeição e a derivação de
// correctable a partir do código de falha (§7).
func TestNewWagerTransactionRejected(t *testing.T) {
	tx := mustTransaction(t, transaction.KindBet, "25.00")

	t.Run("falha_definitiva", func(t *testing.T) {
		event, err := NewWagerTransactionRejected("evt-1", "corr-1", "", tx, transaction.FailureBetInsufficientFunds, eventTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if event.EventType != TypeWagerTransactionRejected {
			t.Errorf("EventType = %q, want %q", event.EventType, TypeWagerTransactionRejected)
		}
		// Saldo insuficiente para apostar é DEFINITIVO para esta operação.
		if event.Data.Correctable {
			t.Errorf("Correctable = true, want false para %s", event.Data.FailureCode)
		}
		if event.Data.FailureCode != transaction.FailureBetInsufficientFunds {
			t.Errorf("FailureCode = %q, want %q", event.Data.FailureCode, transaction.FailureBetInsufficientFunds)
		}
	})

	t.Run("falha_corrigivel", func(t *testing.T) {
		event, err := NewWagerTransactionRejected("evt-2", "corr-1", "", tx, transaction.FailureInvalidAmount, eventTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !event.Data.Correctable {
			t.Errorf("Correctable = false, want true para %s", event.Data.FailureCode)
		}
	})

	t.Run("codigo_invalido", func(t *testing.T) {
		_, err := NewWagerTransactionRejected("evt-3", "corr-1", "", tx, transaction.FailureCode("MADE_UP"), eventTime)
		if !errors.Is(err, transaction.ErrInvalidFailureCode) {
			t.Errorf("error = %v, want transaction.ErrInvalidFailureCode", err)
		}
	})
}

// TestNewWagerTransactionPendingReference exige agendamento de retry (§7).
func TestNewWagerTransactionPendingReference(t *testing.T) {
	t.Run("sem_agendamento_rejeitado", func(t *testing.T) {
		tx := mustTransaction(t, transaction.KindRefund, "25.00")

		_, err := NewWagerTransactionPendingReference("evt-1", "corr-1", "", tx, eventTime)
		if !errors.Is(err, ErrInvalidTimestamp) {
			t.Errorf("error = %v, want ErrInvalidTimestamp", err)
		}
	})

	t.Run("com_agendamento_aceito", func(t *testing.T) {
		tx := mustTransaction(t, transaction.KindRefund, "25.00")
		nextRetry := eventTime.Add(5 * time.Second)

		// Transição REAL pela máquina de estados: PENDING -> PENDING_REFERENCE.
		if err := tx.MarkPendingReference(transaction.FailureReferenceNotFound, nextRetry, eventTime); err != nil {
			t.Fatalf("MarkPendingReference unexpected error: %v", err)
		}

		event, err := NewWagerTransactionPendingReference("evt-1", "corr-1", "", tx, eventTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if event.EventType != TypeWagerTransactionPendingReference {
			t.Errorf("EventType = %q, want %q", event.EventType, TypeWagerTransactionPendingReference)
		}
		if event.Data.Attempts != 1 {
			t.Errorf("Data.Attempts = %d, want 1", event.Data.Attempts)
		}
		if !event.Data.NextRetryAt.Equal(nextRetry) {
			t.Errorf("Data.NextRetryAt = %v, want %v", event.Data.NextRetryAt, nextRetry)
		}
	})
}
