// Testes white-box da transação (§13): máquina de estados par a par, regras dos cinco
// tipos externos (§7), política de valores zero, abertura interna (§9) e hash do
// payload (§9). O pacote é `transaction` para acessar símbolos não exportados.
package transaction

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var txTime = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

const (
	txID         ID                    = "0192f298-345e-7e38-af88-e43f851a819d"
	txWalletID   wallet.ID             = "0192f291-27dd-7d3f-8071-5f8685deef37"
	txPlayerID   wallet.PlayerID       = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	txProviderID ProviderID            = "provider-a"
	txExternalID ExternalTransactionID = "transaction-123"
	txIdemKey    IdempotencyKey        = "provider-a:transaction-123"
	txRoundID                          = "round-987"
	txGameID                           = "fortune-chimp"
)

func mustMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q) unexpected error: %v", amount, err)
	}
	return m
}

func mustHash(t *testing.T) PayloadHash {
	t.Helper()
	hash, err := NewPayloadHash(strings.Repeat("a", payloadHashHexLen))
	if err != nil {
		t.Fatalf("NewPayloadHash unexpected error: %v", err)
	}
	return hash
}

// mustExternal cria uma operação externa válida, permitindo customização.
func mustExternal(t *testing.T, kind Kind, amount string, mutate func(p *NewExternalParams)) *Transaction {
	t.Helper()

	params := NewExternalParams{
		ID:                    txID,
		Kind:                  kind,
		PlayerID:              txPlayerID,
		WalletID:              txWalletID,
		Amount:                mustMoney(t, amount, money.BRL),
		ProviderID:            txProviderID,
		ExternalTransactionID: txExternalID,
		IdempotencyKey:        txIdemKey,
		PayloadHash:           mustHash(t),
		RoundID:               txRoundID,
		GameID:                txGameID,
		Now:                   txTime,
	}
	if kind.IsReversal() {
		params.ReferenceExternalTransactionID = ExternalTransactionID("transaction-000")
	}
	if mutate != nil {
		mutate(&params)
	}

	tx, err := NewExternal(params)
	if err != nil {
		t.Fatalf("NewExternal(%s) unexpected error: %v", kind, err)
	}
	return tx
}

// TestParseKind cobre o vocabulário de tipos.
func TestParseKind(t *testing.T) {
	valid := []string{"OPENING", "BET", "WIN", "LOSS", "REFUND", "ROLLBACK"}
	for _, raw := range valid {
		t.Run("valido_"+raw, func(t *testing.T) {
			kind, err := ParseKind(raw)
			if err != nil {
				t.Fatalf("ParseKind(%q) unexpected error: %v", raw, err)
			}
			if kind.String() != raw {
				t.Errorf("String() = %q, want %q", kind.String(), raw)
			}
		})
	}

	invalid := []string{"", "bet", "Bet", "DESCONHECIDO", "BET ", " BET", "BETT"}
	for _, raw := range invalid {
		t.Run("invalido_"+raw, func(t *testing.T) {
			// Não normalizamos caixa: "bet" é rejeitado em vez de virar "BET",
			// porque normalizar silenciosamente mudaria o hash de idempotência.
			if _, err := ParseKind(raw); !errors.Is(err, ErrInvalidKind) {
				t.Errorf("ParseKind(%q) error = %v, want ErrInvalidKind", raw, err)
			}
		})
	}
}

// TestKindRules cobre a política de VALOR ZERO e a classificação por tipo (§7).
func TestKindRules(t *testing.T) {
	cases := []struct {
		kind        Kind
		allowsZero  bool
		isExternal  bool
		isReversal  bool
		movesWallet bool
	}{
		// OPENING: interno, aceita zero (mas zero NÃO gera OPENING no fluxo), move carteira.
		{kind: KindOpening, allowsZero: true, isExternal: false, isReversal: false, movesWallet: true},
		// BET: externo, exige > 0, move a carteira (débito).
		{kind: KindBet, allowsZero: false, isExternal: true, isReversal: false, movesWallet: true},
		// WIN: externo, exige > 0, move a carteira (crédito).
		{kind: KindWin, allowsZero: false, isExternal: true, isReversal: false, movesWallet: true},
		// LOSS: único externo que NÃO move a carteira e EXIGE valor zero (§7).
		{kind: KindLoss, allowsZero: true, isExternal: true, isReversal: false, movesWallet: false},
		// REFUND: reversão, exige > 0.
		{kind: KindRefund, allowsZero: false, isExternal: true, isReversal: true, movesWallet: true},
		// ROLLBACK: reversão, exige > 0.
		{kind: KindRollback, allowsZero: false, isExternal: true, isReversal: true, movesWallet: true},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := tc.kind.AllowsZeroAmount(); got != tc.allowsZero {
				t.Errorf("AllowsZeroAmount() = %v, want %v", got, tc.allowsZero)
			}
			if got := tc.kind.RequiresPositiveAmount(); got == tc.allowsZero {
				t.Errorf("RequiresPositiveAmount() inconsistente com AllowsZeroAmount()")
			}
			if got := tc.kind.IsExternal(); got != tc.isExternal {
				t.Errorf("IsExternal() = %v, want %v", got, tc.isExternal)
			}
			if got := tc.kind.IsReversal(); got != tc.isReversal {
				t.Errorf("IsReversal() = %v, want %v", got, tc.isReversal)
			}
			if got := tc.kind.MovesWallet(); got != tc.movesWallet {
				t.Errorf("MovesWallet() = %v, want %v", got, tc.movesWallet)
			}
		})
	}
}

// TestAllowsReferenceFrom: REFUND só reverte BET; ROLLBACK reverte BET, WIN ou REFUND (§7).
func TestAllowsReferenceFrom(t *testing.T) {
	cases := []struct {
		referenced Kind
		reversal   Kind
		want       bool
	}{
		{referenced: KindBet, reversal: KindRefund, want: true},
		{referenced: KindWin, reversal: KindRefund, want: false},
		{referenced: KindLoss, reversal: KindRefund, want: false},
		{referenced: KindRefund, reversal: KindRefund, want: false},
		{referenced: KindBet, reversal: KindRollback, want: true},
		{referenced: KindWin, reversal: KindRollback, want: true},
		{referenced: KindRefund, reversal: KindRollback, want: true},
		{referenced: KindLoss, reversal: KindRollback, want: false},
		{referenced: KindBet, reversal: KindBet, want: false},
	}

	for _, tc := range cases {
		t.Run(string(tc.reversal)+"_de_"+string(tc.referenced), func(t *testing.T) {
			if got := tc.referenced.AllowsReferenceFrom(tc.reversal); got != tc.want {
				t.Errorf("AllowsReferenceFrom() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStateMachine_Complete valida a máquina de estados par a par, cobrindo inclusive
// transições que a API pública não expõe.
func TestStateMachine_Complete(t *testing.T) {
	allStates := []State{StatePending, StatePendingReference, StateProcessed, StateRejected, StateFailed}

	// Pares permitidos, exatamente como a tabela `allowedTransitions`.
	allowed := map[State]map[State]bool{
		StatePending: {
			StatePendingReference: true,
			StateProcessed:        true,
			StateRejected:         true,
			StateFailed:           true,
		},
		StatePendingReference: {
			StatePendingReference: true, // rearmar a espera (§7)
			StateProcessed:        true,
			StateRejected:         true,
			StateFailed:           true,
		},
	}

	for _, from := range allStates {
		for _, to := range allStates {
			name := string(from) + "_para_" + string(to)
			t.Run(name, func(t *testing.T) {
				want := allowed[from][to]
				if got := from.CanTransitionTo(to); got != want {
					t.Errorf("CanTransitionTo = %v, want %v", got, want)
				}
			})
		}
	}
}

// TestStateTerminal prova que os três estados finais não têm saída (§6.3).
func TestStateTerminal(t *testing.T) {
	terminals := []State{StateProcessed, StateRejected, StateFailed}
	for _, state := range terminals {
		t.Run(string(state), func(t *testing.T) {
			if !state.IsTerminal() {
				t.Errorf("IsTerminal() = false, want true")
			}
		})
	}

	// Estados não terminais.
	for _, state := range []State{StatePending, StatePendingReference} {
		if state.IsTerminal() {
			t.Errorf("%s.IsTerminal() = true, want false", state)
		}
	}
}

// TestNewExternal_Valid cobre a criação de operações externas.
func TestNewExternal_Valid(t *testing.T) {
	t.Run("bet_inicial_pending", func(t *testing.T) {
		tx := mustExternal(t, KindBet, "25.00", nil)

		if tx.State() != StatePending {
			t.Errorf("State() = %q, want %q", tx.State(), StatePending)
		}
		if tx.Origin() != OriginExternal {
			t.Errorf("Origin() = %q, want %q", tx.Origin(), OriginExternal)
		}
		if tx.Attempts() != 0 {
			t.Errorf("Attempts() = %d, want 0", tx.Attempts())
		}
		if tx.IsTerminal() {
			t.Errorf("IsTerminal() = true, want false")
		}
		if tx.HasResolvedReference() {
			t.Errorf("HasResolvedReference() = true, want false")
		}
	})

	t.Run("loss_com_valor_zero", func(t *testing.T) {
		// §7: LOSS exige exatamente "0.00" e não movimenta a carteira.
		tx := mustExternal(t, KindLoss, "0.00", nil)

		if !tx.Amount().IsZero() {
			t.Errorf("Amount() = %s, want 0.00", tx.Amount())
		}
		if tx.Kind().MovesWallet() {
			t.Errorf("LOSS nao deve mover a carteira")
		}
	})
}

// TestNewExternal_Invalid cobre todas as rejeições de criação.
func TestNewExternal_Invalid(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		amount string
		mutate func(p *NewExternalParams)
		want   error
	}{
		{
			name: "opening_por_canal_externo",
			kind: KindOpening, amount: "25.00",
			want: ErrOpeningNotAllowedExternally,
		},
		{
			name: "loss_com_valor_positivo",
			kind: KindLoss, amount: "25.00",
			want: ErrLossAmountNotZero,
		},
		{
			name: "bet_com_valor_zero",
			kind: KindBet, amount: "0.00",
			want: ErrInvalidAmount,
		},
		{
			name: "refund_sem_referencia",
			kind: KindRefund, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.ReferenceExternalTransactionID = "" },
			want:   ErrMissingReference,
		},
		{
			name: "rollback_sem_referencia",
			kind: KindRollback, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.ReferenceExternalTransactionID = "" },
			want:   ErrMissingReference,
		},
		{
			name: "bet_com_referencia_indevida",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.ReferenceExternalTransactionID = "transaction-999" },
			want:   ErrUnexpectedReference,
		},
		{
			name: "sem_provider",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.ProviderID = "" },
			want:   ErrMissingExternalMetadata,
		},
		{
			name: "sem_external_id",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.ExternalTransactionID = "" },
			want:   ErrMissingExternalMetadata,
		},
		{
			name: "sem_idempotency_key",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.IdempotencyKey = "" },
			want:   ErrMissingExternalMetadata,
		},
		{
			name: "sem_payload_hash",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.PayloadHash = "" },
			want:   ErrMissingExternalMetadata,
		},
		{
			name: "sem_round_id",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.RoundID = "" },
			want:   ErrMissingExternalMetadata,
		},
		{
			name: "sem_game_id",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.GameID = "" },
			want:   ErrMissingExternalMetadata,
		},
		{
			name: "sem_timestamp",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.Now = time.Time{} },
			want:   ErrInvalidTimestamp,
		},
		{
			name: "sem_moeda_no_valor",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.Amount = money.Money{} },
			want:   money.ErrUninitialized,
		},
		{
			name: "tipo_invalido",
			kind: Kind("NOPE"), amount: "25.00",
			want: ErrInvalidKind,
		},
		{
			name: "id_vazio",
			kind: KindBet, amount: "25.00",
			mutate: func(p *NewExternalParams) { p.ID = "" },
			want:   ErrInvalidID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := NewExternalParams{
				ID:                    txID,
				Kind:                  tc.kind,
				PlayerID:              txPlayerID,
				WalletID:              txWalletID,
				Amount:                mustMoney(t, tc.amount, money.BRL),
				ProviderID:            txProviderID,
				ExternalTransactionID: txExternalID,
				IdempotencyKey:        txIdemKey,
				PayloadHash:           mustHash(t),
				RoundID:               txRoundID,
				GameID:                txGameID,
				Now:                   txTime,
			}
			if tc.kind.IsReversal() {
				params.ReferenceExternalTransactionID = ExternalTransactionID("transaction-000")
			}
			if tc.mutate != nil {
				tc.mutate(&params)
			}

			_, err := NewExternal(params)
			if err == nil {
				t.Fatalf("NewExternal expected error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want sentinel %v", err, tc.want)
			}
		})
	}
}

// TestNewOpening cobre a abertura interna (§9).
func TestNewOpening(t *testing.T) {
	t.Run("abertura_positiva_ja_processada", func(t *testing.T) {
		tx, err := NewOpening(NewOpeningParams{
			ID:       txID,
			PlayerID: txPlayerID,
			WalletID: txWalletID,
			Amount:   mustMoney(t, "1000.00", money.BRL),
			Now:      txTime,
		})
		if err != nil {
			t.Fatalf("NewOpening unexpected error: %v", err)
		}

		// §9: OPENING nasce em PROCESSED (não há etapa assíncrona na abertura).
		if tx.State() != StateProcessed {
			t.Errorf("State() = %q, want %q", tx.State(), StateProcessed)
		}
		if tx.Origin() != OriginInternal {
			t.Errorf("Origin() = %q, want %q", tx.Origin(), OriginInternal)
		}
		if tx.Kind() != KindOpening {
			t.Errorf("Kind() = %q, want %q", tx.Kind(), KindOpening)
		}
		// §6.3: abertura interna NÃO carrega metadados externos.
		if tx.ProviderID() != "" || tx.ExternalTransactionID() != "" ||
			tx.IdempotencyKey() != "" || tx.RoundID() != "" || tx.GameID() != "" {
			t.Errorf("abertura interna nao deve carregar metadados externos")
		}
		// O resultado financeiro devolvido é o próprio valor creditado.
		if result := tx.ResultBalance(); !result.Valid || result.Value.String() != "1000.00" {
			t.Errorf("ResultBalance() = %+v, want 1000.00", result)
		}
		if tx.ProcessedAt().IsZero() {
			t.Errorf("ProcessedAt() = zero, want set")
		}
	})

	t.Run("abertura_zero_e_rejeitada", func(t *testing.T) {
		// §9: saldo inicial zero NÃO cria OPENING. O construtor recusa.
		_, err := NewOpening(NewOpeningParams{
			ID:       txID,
			PlayerID: txPlayerID,
			WalletID: txWalletID,
			Amount:   mustMoney(t, "0.00", money.BRL),
			Now:      txTime,
		})
		if !errors.Is(err, ErrOpeningRequiresPositiveAmount) {
			t.Errorf("error = %v, want ErrOpeningRequiresPositiveAmount", err)
		}
	})
}

// TestTransitions cobre os métodos explícitos de transição.
func TestTransitions(t *testing.T) {
	t.Run("processar_e_nao_reprocessar", func(t *testing.T) {
		tx := mustExternal(t, KindBet, "25.00", nil)

		if err := tx.MarkProcessed(mustMoney(t, "975.00", money.BRL), txTime); err != nil {
			t.Fatalf("MarkProcessed unexpected error: %v", err)
		}
		if !tx.IsTerminal() {
			t.Errorf("IsTerminal() = false apos PROCESSED")
		}
		// §6.3: estado terminal não transiciona — nem para si mesmo.
		// Replay consulta o resultado persistido, NÃO reaplica.
		if err := tx.MarkProcessed(mustMoney(t, "975.00", money.BRL), txTime); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("segundo MarkProcessed error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("rejeitar_com_codigo", func(t *testing.T) {
		tx := mustExternal(t, KindBet, "25.00", nil)

		if err := tx.MarkRejected(FailureBetInsufficientFunds, txTime); err != nil {
			t.Fatalf("MarkRejected unexpected error: %v", err)
		}
		if tx.State() != StateRejected {
			t.Errorf("State() = %q, want %q", tx.State(), StateRejected)
		}
		if tx.FailureCode() != FailureBetInsufficientFunds {
			t.Errorf("FailureCode() = %q, want %q", tx.FailureCode(), FailureBetInsufficientFunds)
		}
	})

	t.Run("rejeitar_com_codigo_invalido", func(t *testing.T) {
		tx := mustExternal(t, KindBet, "25.00", nil)

		if err := tx.MarkRejected(FailureCode("INVENTADO"), txTime); !errors.Is(err, ErrInvalidFailureCode) {
			t.Errorf("error = %v, want ErrInvalidFailureCode", err)
		}
		// Estado intacto após tentativa inválida.
		if tx.State() != StatePending {
			t.Errorf("State() = %q, want %q", tx.State(), StatePending)
		}
	})

	t.Run("falhar_permanentemente", func(t *testing.T) {
		tx := mustExternal(t, KindWin, "25.00", nil)

		if err := tx.MarkFailed(FailureInternal, txTime); err != nil {
			t.Fatalf("MarkFailed unexpected error: %v", err)
		}
		if tx.State() != StateFailed {
			t.Errorf("State() = %q, want %q", tx.State(), StateFailed)
		}
	})

	t.Run("pending_reference_rearma_espera", func(t *testing.T) {
		tx := mustExternal(t, KindRefund, "25.00", nil)
		first := txTime.Add(2 * time.Second)

		if err := tx.MarkPendingReference(FailureReferenceNotFound, first, txTime); err != nil {
			t.Fatalf("primeiro MarkPendingReference unexpected error: %v", err)
		}
		if tx.State() != StatePendingReference {
			t.Errorf("State() = %q, want %q", tx.State(), StatePendingReference)
		}
		if tx.Attempts() != 1 {
			t.Errorf("Attempts() = %d, want 1", tx.Attempts())
		}

		// Segunda tentativa: rearma a espera (auto-transição permitida).
		second := first.Add(4 * time.Second)
		if err := tx.MarkPendingReference(FailureReferenceNotFound, second, first); err != nil {
			t.Fatalf("segundo MarkPendingReference unexpected error: %v", err)
		}
		if tx.Attempts() != 2 {
			t.Errorf("Attempts() = %d, want 2", tx.Attempts())
		}
		if !tx.NextRetryAt().Equal(second) {
			t.Errorf("NextRetryAt() = %v, want %v", tx.NextRetryAt(), second)
		}

		// E ainda pode concluir a partir de PENDING_REFERENCE.
		if err := tx.MarkProcessed(mustMoney(t, "975.00", money.BRL), second); err != nil {
			t.Fatalf("MarkProcessed a partir de PENDING_REFERENCE error: %v", err)
		}
	})

	t.Run("terminal_nao_vai_para_pending_reference", func(t *testing.T) {
		tx := mustExternal(t, KindBet, "25.00", nil)
		if err := tx.MarkRejected(FailureBetInsufficientFunds, txTime); err != nil {
			t.Fatalf("MarkRejected unexpected error: %v", err)
		}

		err := tx.MarkPendingReference(FailureReferenceNotFound, txTime.Add(time.Minute), txTime)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("anexar_referencia_resolvida", func(t *testing.T) {
		tx := mustExternal(t, KindRefund, "25.00", nil)

		referenceID := ID("0192f200-0000-7000-8000-000000000001")
		if err := tx.AttachResolvedReference(referenceID, txTime); err != nil {
			t.Fatalf("AttachResolvedReference unexpected error: %v", err)
		}
		if !tx.HasResolvedReference() {
			t.Errorf("HasResolvedReference() = false, want true")
		}
		if got := tx.ReferenceTransactionID().Value; got != referenceID {
			t.Errorf("ReferenceTransactionID = %q, want %q", got, referenceID)
		}
	})

	t.Run("nil_receiver_nao_faz_panic", func(t *testing.T) {
		var tx *Transaction

		if err := tx.MarkProcessed(mustMoney(t, "1.00", money.BRL), txTime); !errors.Is(err, ErrUninitialized) {
			t.Errorf("MarkProcessed error = %v, want ErrUninitialized", err)
		}
		if err := tx.MarkRejected(FailureInternal, txTime); !errors.Is(err, ErrUninitialized) {
			t.Errorf("MarkRejected error = %v, want ErrUninitialized", err)
		}
	})
}

// TestRehydrate valida a reconstrução a partir do estado persistido (§6).
func TestRehydrate(t *testing.T) {
	base := RehydrateParams{
		ID:                     txID,
		Origin:                 OriginExternal,
		Kind:                   KindBet,
		State:                  StatePending,
		PlayerID:               txPlayerID,
		WalletID:               txWalletID,
		Amount:                 mustMoney(t, "25.00", money.BRL),
		ProviderID:             txProviderID,
		ExternalTransactionID:  txExternalID,
		IdempotencyKey:         txIdemKey,
		PayloadHash:            mustHash(t),
		RoundID:                txRoundID,
		GameID:                 txGameID,
		ReferenceTransactionID: NoID(),
		ResultBalance:          NoMoney(),
		CreatedAt:              txTime,
		UpdatedAt:              txTime,
	}

	t.Run("estado_preservado_sem_transicao", func(t *testing.T) {
		// Reidrata uma transação já REJECTED; se a reidratação reaplicasse algo, o estado mudaria.
		params := base
		params.State = StateRejected
		params.FailureCode = FailureBetInsufficientFunds
		params.ProcessedAt = txTime

		tx, err := Rehydrate(params)
		if err != nil {
			t.Fatalf("Rehydrate unexpected error: %v", err)
		}
		if tx.State() != StateRejected {
			t.Errorf("State() = %q, want %q", tx.State(), StateRejected)
		}
		if tx.FailureCode() != FailureBetInsufficientFunds {
			t.Errorf("FailureCode() = %q, want %q", tx.FailureCode(), FailureBetInsufficientFunds)
		}
	})

	t.Run("rejeicoes_estruturais", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(p *RehydrateParams)
			want   error
		}{
			{name: "id_vazio", mutate: func(p *RehydrateParams) { p.ID = "" }, want: ErrInvalidID},
			{name: "kind_invalido", mutate: func(p *RehydrateParams) { p.Kind = "NOPE" }, want: ErrInvalidKind},
			{name: "state_invalido", mutate: func(p *RehydrateParams) { p.State = "WEIRD" }, want: ErrInvalidState},
			{name: "valor_zero_em_bet", mutate: func(p *RehydrateParams) { p.Amount = mustMoney(t, "0.00", money.BRL) }, want: ErrInvalidAmount},
			{name: "timestamps_ausentes", mutate: func(p *RehydrateParams) { p.CreatedAt = time.Time{} }, want: ErrInvalidTimestamp},
			{name: "attempts_negativas", mutate: func(p *RehydrateParams) { p.Attempts = -1 }, want: ErrNegativeAttempts},
			{name: "failure_code_invalido", mutate: func(p *RehydrateParams) { p.FailureCode = "NOPE" }, want: ErrInvalidFailureCode},
			{name: "interno_que_nao_e_opening", mutate: func(p *RehydrateParams) {
				p.Origin = OriginInternal
			}, want: ErrInconsistentOrigin},
		}

		// Cada caso espera um sentinela específico (errors.Is), não o texto do erro.
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				params := base
				tc.mutate(&params)

				_, err := Rehydrate(params)
				if err == nil {
					t.Fatalf("Rehydrate expected error, got nil")
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("error = %v, want sentinel %v", err, tc.want)
				}
			})
		}
	})

	t.Run("externo_sem_metadados", func(t *testing.T) {
		params := base
		params.ProviderID = ""

		if _, err := Rehydrate(params); !errors.Is(err, ErrMissingExternalMetadata) {
			t.Errorf("error = %v, want ErrMissingExternalMetadata", err)
		}
	})
}

// TestComputePayloadHash cobre o hash determinístico dos campos de negócio (§9).
func TestComputePayloadHash(t *testing.T) {
	fields := CanonicalPayloadFields{
		ProviderID:            txProviderID,
		ExternalTransactionID: txExternalID,
		PlayerID:              txPlayerID,
		WalletID:              txWalletID,
		RoundID:               txRoundID,
		GameID:                txGameID,
		Kind:                  KindBet,
		Amount:                mustMoney(t, "25.00", money.BRL),
	}

	t.Run("deterministico", func(t *testing.T) {
		first, err := ComputePayloadHash(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		second, err := ComputePayloadHash(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// HTTP e SQS compartilham esta função: o mesmo conteúdo gera o mesmo hash.
		if first != second {
			t.Errorf("hash nao deterministico: %q != %q", first, second)
		}
		if len(first.String()) != payloadHashHexLen {
			t.Errorf("hash length = %d, want %d", len(first.String()), payloadHashHexLen)
		}
	})

	t.Run("normalizacao_antes_do_hash", func(t *testing.T) {
		// "25", "25.0" e "25.00" representam o MESMO valor. Depois da
		// normalização do Money, produzem o MESMO hash (§9).
		variants := []string{"25", "25.0", "25.00"}
		var hashes []PayloadHash

		for _, amount := range variants {
			variant := fields
			variant.Amount = mustMoney(t, amount, money.BRL)

			hash, err := ComputePayloadHash(variant)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			hashes = append(hashes, hash)
		}

		for i := 1; i < len(hashes); i++ {
			if hashes[i] != hashes[0] {
				t.Errorf("variante %q gerou hash diferente: %q != %q", variants[i], hashes[i], hashes[0])
			}
		}
	})

	t.Run("conteudo_diferente_gera_hash_diferente", func(t *testing.T) {
		original, err := ComputePayloadHash(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Mesma chave de idempotência, mas VALOR diferente -> conflito (§9).
		changed := fields
		changed.Amount = mustMoney(t, "25.01", money.BRL)

		divergent, err := ComputePayloadHash(changed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if original == divergent {
			t.Errorf("conteudos diferentes nao podem gerar o mesmo hash")
		}
	})

	t.Run("golden_bytes", func(t *testing.T) {
		// GOLDEN: congela o JSON canônico — reordenar campos ou renomear uma tag quebra
		// este teste, protegendo a idempotência de operações já persistidas.
		payload, err := CanonicalPayload(fields)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		const want = `{"amount":"25.00","currency":"BRL","externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","providerId":"provider-a","referenceExternalTransactionId":"","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}`

		if string(payload) != want {
			t.Errorf("payload canonico divergiu.\n got: %s\nwant: %s", payload, want)
		}
	})
}

// TestFailureCodeClassification: distingue corrigíveis de definitivos e comprova que os
// dois códigos de saldo são diferentes (§7).
func TestFailureCodeClassification(t *testing.T) {
	t.Run("codigos_de_saldo_sao_distintos", func(t *testing.T) {
		// §7: o código de "reversão sem saldo" deve ser DIFERENTE do de
		// "aposta sem saldo".
		if FailureBetInsufficientFunds == FailureReversalInsufficientFunds {
			t.Errorf("os codigos de saldo devem ser distintos")
		}
		if !FailureBetInsufficientFunds.IsValid() || !FailureReversalInsufficientFunds.IsValid() {
			t.Errorf("codigos de saldo devem pertencer a lista fechada")
		}
	})

	t.Run("corrigiveis", func(t *testing.T) {
		correctable := []FailureCode{
			FailureInvalidAmount,
			FailureInvalidKind,
			FailureCurrencyMismatch,
			FailureAmountMismatch,
			FailureReferenceMismatch,
			FailureReferenceNotFound,
			FailureReferenceStillPending,
		}
		for _, code := range correctable {
			if !code.IsCorrectable() {
				t.Errorf("%s deveria ser corrigivel", code)
			}
		}
	})

	t.Run("definitivos", func(t *testing.T) {
		definitive := []FailureCode{
			FailureBetInsufficientFunds,
			FailureReversalInsufficientFunds,
			FailureDuplicateReversal,
			FailureReferenceKindNotAllowed,
			FailureReferenceNotProcessed,
			FailureWalletNotFound,
			FailureIdempotencyConflict,
			FailureInternal,
		}
		for _, code := range definitive {
			if code.IsCorrectable() {
				t.Errorf("%s deveria ser definitivo", code)
			}
		}
	})

	t.Run("codigo_inventado_invalido", func(t *testing.T) {
		if FailureCode("NAO_EXISTE").IsValid() {
			t.Errorf("codigo fora da lista fechada deve ser invalido")
		}
	})
}
