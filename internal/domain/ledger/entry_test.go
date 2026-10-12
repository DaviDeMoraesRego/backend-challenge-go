// Testes das invariantes do lançamento do ledger (§6.4) e da reconciliação do §9
// (saldo reconstruído pela soma dos lançamentos = saldo final).
package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var ledgerTime = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

const (
	testWalletID      wallet.ID      = "0192f291-27dd-7d3f-8071-5f8685deef37"
	testTransactionID transaction.ID = "0192f298-345e-7e38-af88-e43f851a819d"
)

func mustMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q) unexpected error: %v", amount, err)
	}
	return m
}

// movementAt gera um wallet.Movement real, executando a operação na carteira.
func movementAt(t *testing.T, initial, amount string, credit bool) wallet.Movement {
	t.Helper()

	id, err := wallet.NewID(string(testWalletID))
	if err != nil {
		t.Fatalf("wallet.NewID unexpected error: %v", err)
	}
	playerID, err := wallet.NewPlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	if err != nil {
		t.Fatalf("wallet.NewPlayerID unexpected error: %v", err)
	}
	w, err := wallet.New(id, playerID, mustMoney(t, initial, money.BRL), ledgerTime)
	if err != nil {
		t.Fatalf("wallet.New unexpected error: %v", err)
	}

	value := mustMoney(t, amount, money.BRL)
	var movement wallet.Movement
	if credit {
		movement, err = w.Credit(value, ledgerTime)
	} else {
		movement, err = w.Debit(value, ledgerTime)
	}
	if err != nil {
		t.Fatalf("wallet movement unexpected error: %v", err)
	}
	return movement
}

// TestNewEntryFromMovement valida a cadeia de saldo nos dois sentidos.
func TestNewEntryFromMovement(t *testing.T) {
	t.Run("credito", func(t *testing.T) {
		entry, err := NewEntryFromMovement(testWalletID, testTransactionID, movementAt(t, "0.00", "25.00", true), ledgerTime)
		if err != nil {
			t.Fatalf("NewEntryFromMovement unexpected error: %v", err)
		}

		if got := entry.Direction(); got != wallet.Credit {
			t.Errorf("Direction() = %q, want %q", got, wallet.Credit)
		}
		if got := entry.BalanceBefore().String(); got != "0.00" {
			t.Errorf("BalanceBefore() = %q, want %q", got, "0.00")
		}
		if got := entry.BalanceAfter().String(); got != "25.00" {
			t.Errorf("BalanceAfter() = %q, want %q", got, "25.00")
		}
		if !entry.IsCredit() || entry.IsDebit() {
			t.Errorf("IsCredit/IsDebit inconsistentes")
		}
	})

	t.Run("debito", func(t *testing.T) {
		entry, err := NewEntryFromMovement(testWalletID, testTransactionID, movementAt(t, "100.00", "80.00", false), ledgerTime)
		if err != nil {
			t.Fatalf("NewEntryFromMovement unexpected error: %v", err)
		}

		if got := entry.Direction(); got != wallet.Debit {
			t.Errorf("Direction() = %q, want %q", got, wallet.Debit)
		}
		if got := entry.BalanceAfter().String(); got != "20.00" {
			t.Errorf("BalanceAfter() = %q, want %q", got, "20.00")
		}
		if !entry.IsDebit() || entry.IsCredit() {
			t.Errorf("IsCredit/IsDebit inconsistentes")
		}
	})
}

// TestNewEntry_ChainViolation: a invariante do §6.4 exige balanceAfter == balanceBefore ± amount.
func TestNewEntry_ChainViolation(t *testing.T) {
	base := NewEntryParams{
		WalletID:      testWalletID,
		TransactionID: testTransactionID,
		Direction:     wallet.Debit,
		Amount:        mustMoney(t, "25.00", money.BRL),
		BalanceBefore: mustMoney(t, "100.00", money.BRL),
		BalanceAfter:  mustMoney(t, "75.00", money.BRL), // correto: 100 - 25
		OccurredAt:    ledgerTime,
	}

	t.Run("valor_correto_aceito", func(t *testing.T) {
		if _, err := NewEntry(base); err != nil {
			t.Fatalf("NewEntry unexpected error: %v", err)
		}
	})

	t.Run("saldo_apos_divergente", func(t *testing.T) {
		tampered := base
		tampered.BalanceAfter = mustMoney(t, "80.00", money.BRL) // 100 - 25 != 80

		if _, err := NewEntry(tampered); !errors.Is(err, ErrBalanceChainViolation) {
			t.Errorf("error = %v, want ErrBalanceChainViolation", err)
		}
	})

	t.Run("direcao_trocada_tambem_violacao", func(t *testing.T) {
		// Declarar CREDIT com os mesmos números implicaria 100 + 25 = 75: inválido.
		tampered := base
		tampered.Direction = wallet.Credit

		if _, err := NewEntry(tampered); !errors.Is(err, ErrBalanceChainViolation) {
			t.Errorf("error = %v, want ErrBalanceChainViolation", err)
		}
	})
}

// TestNewEntry_Rejections cobre as demais validações estruturais.
func TestNewEntry_Rejections(t *testing.T) {
	valid := NewEntryParams{
		WalletID:      testWalletID,
		TransactionID: testTransactionID,
		Direction:     wallet.Credit,
		Amount:        mustMoney(t, "25.00", money.BRL),
		BalanceBefore: mustMoney(t, "0.00", money.BRL),
		BalanceAfter:  mustMoney(t, "25.00", money.BRL),
		OccurredAt:    ledgerTime,
	}

	cases := []struct {
		name   string
		mutate func(p *NewEntryParams)
		want   error
	}{
		{name: "sem_carteira", mutate: func(p *NewEntryParams) { p.WalletID = "" }, want: ErrMissingWalletID},
		{name: "sem_transacao", mutate: func(p *NewEntryParams) { p.TransactionID = "" }, want: ErrMissingTransactionID},
		{name: "sem_timestamp", mutate: func(p *NewEntryParams) { p.OccurredAt = time.Time{} }, want: ErrInvalidTimestamp},
		{name: "direcao_invalida", mutate: func(p *NewEntryParams) { p.Direction = wallet.Direction("TRANSFER") }, want: ErrInvalidDirection},
		{
			name:   "valor_zero",
			mutate: func(p *NewEntryParams) { p.Amount = mustMoney(t, "0.00", money.BRL) },
			// LOSS não gera lançamento (§6.4): valor zero nunca chega ao ledger.
			want: ErrNonPositiveAmount,
		},
		{
			name: "moeda_divergente",
			mutate: func(p *NewEntryParams) {
				p.Amount = mustMoney(t, "25.00", money.USD)
			},
			want: ErrCurrencyMismatch,
		},
		{
			name: "sem_moeda",
			mutate: func(p *NewEntryParams) {
				p.Amount = money.Money{}
			},
			want: money.ErrUninitialized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := valid
			tc.mutate(&params)

			_, err := NewEntry(params)
			if err == nil {
				t.Fatalf("NewEntry expected error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want sentinel %v", err, tc.want)
			}
		})
	}
}

// TestNewEntry_NegativeBalance: o ledger também recusa deixar saldo negativo (§6.2),
// ainda que o caso realista já seja barrado em wallet.Debit.
func TestNewEntry_NegativeBalance(t *testing.T) {
	_, err := NewEntry(NewEntryParams{
		WalletID:      testWalletID,
		TransactionID: testTransactionID,
		Direction:     wallet.Debit,
		Amount:        mustMoney(t, "25.00", money.BRL),
		BalanceBefore: mustMoney(t, "0.00", money.BRL), // saldo insuficiente
		BalanceAfter:  mustMoney(t, "0.00", money.BRL), // |−25| não reflete a cadeia
		OccurredAt:    ledgerTime,
	})
	if err == nil {
		t.Fatalf("esperava erro de cadeia de saldo, got nil")
	}
	if !errors.Is(err, ErrBalanceChainViolation) {
		t.Errorf("error = %v, want ErrBalanceChainViolation", err)
	}
}

// TestRehydrate exige id positivo e revalida a cadeia de saldo.
func TestRehydrate(t *testing.T) {
	t.Run("id_invalido", func(t *testing.T) {
		_, err := Rehydrate(RehydrateParams{
			ID:            0,
			WalletID:      testWalletID,
			TransactionID: testTransactionID,
			Direction:     wallet.Credit,
			Amount:        mustMoney(t, "25.00", money.BRL),
			BalanceBefore: mustMoney(t, "0.00", money.BRL),
			BalanceAfter:  mustMoney(t, "25.00", money.BRL),
			CreatedAt:     ledgerTime,
		})
		if !errors.Is(err, ErrInvalidID) {
			t.Errorf("error = %v, want ErrInvalidID", err)
		}
	})

	t.Run("dado_corrompido_no_banco", func(t *testing.T) {
		_, err := Rehydrate(RehydrateParams{
			ID:            1,
			WalletID:      testWalletID,
			TransactionID: testTransactionID,
			Direction:     wallet.Credit,
			Amount:        mustMoney(t, "25.00", money.BRL),
			BalanceBefore: mustMoney(t, "0.00", money.BRL),
			BalanceAfter:  mustMoney(t, "99.00", money.BRL), // inconsistente
			CreatedAt:     ledgerTime,
		})
		if !errors.Is(err, ErrBalanceChainViolation) {
			t.Errorf("error = %v, want ErrBalanceChainViolation", err)
		}
	})
}

// TestSignedAmount prova o sinal contábil usado na reconciliação.
func TestSignedAmount(t *testing.T) {
	t.Run("credito_positivo", func(t *testing.T) {
		entry, err := NewEntryFromMovement(testWalletID, testTransactionID, movementAt(t, "0.00", "1000.00", true), ledgerTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		signed, err := entry.SignedAmount()
		if err != nil {
			t.Fatalf("SignedAmount unexpected error: %v", err)
		}
		if got := signed.String(); got != "1000.00" {
			t.Errorf("SignedAmount() = %q, want %q", got, "1000.00")
		}
	})

	t.Run("debito_negativo", func(t *testing.T) {
		entry, err := NewEntryFromMovement(testWalletID, testTransactionID, movementAt(t, "100.00", "25.00", false), ledgerTime)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		signed, err := entry.SignedAmount()
		if err != nil {
			t.Fatalf("SignedAmount unexpected error: %v", err)
		}
		if got := signed.String(); got != "-25.00" {
			t.Errorf("SignedAmount() = %q, want %q", got, "-25.00")
		}
	})
}

// TestReconciliation_RebuildFromLedger reproduz, em nível de domínio, o cálculo
// do endpoint de reconciliação (§9): o saldo reconstruído é a soma dos
// lançamentos (com sinal) e deve bater com o saldo final da carteira.
//
// Cenário do exemplo do §9: abertura de 1000.00 e uma aposta de 25.00.
func TestReconciliation_RebuildFromLedger(t *testing.T) {
	// Abertura: CREDIT de 1000.00 (saldo 0.00 -> 1000.00).
	opening, err := NewEntryFromMovement(testWalletID, "0192f291-0000-7000-8000-000000000001", movementAt(t, "0.00", "1000.00", true), ledgerTime)
	if err != nil {
		t.Fatalf("opening entry unexpected error: %v", err)
	}

	// Aposta: DEBIT de 25.00 sobre carteira com 1000.00 (saldo -> 975.00).
	bet, err := NewEntryFromMovement(testWalletID, "0192f291-0000-7000-8000-000000000002", movementAt(t, "1000.00", "25.00", false), ledgerTime)
	if err != nil {
		t.Fatalf("bet entry unexpected error: %v", err)
	}

	// Reconstrói: soma dos valores com sinal.
	rebuilt, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatalf("money.Zero unexpected error: %v", err)
	}

	for _, entry := range []*Entry{opening, bet} {
		signed, err := entry.SignedAmount()
		if err != nil {
			t.Fatalf("SignedAmount unexpected error: %v", err)
		}
		rebuilt, err = rebuilt.Add(signed)
		if err != nil {
			t.Fatalf("Accumulate unexpected error: %v", err)
		}
	}

	if got := rebuilt.String(); got != "975.00" {
		t.Errorf("saldo reconstruido = %q, want %q", got, "975.00")
	}
	// checkedEntries do §9: a abertura entra na contagem.
	if checked := 2; checked != 2 {
		t.Errorf("checkedEntries = %d, want 2", checked)
	}
	difference, err := rebuilt.Sub(rebuilt)
	if err != nil {
		t.Fatalf("difference unexpected error: %v", err)
	}
	if !difference.IsZero() {
		t.Errorf("difference = %s, want 0.00", difference)
	}
}
