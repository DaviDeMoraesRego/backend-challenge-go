// Testes das invariantes do agregado Wallet (§6.2/§13): criação, reidratação sem
// reaplicar movimentações, débito preservando saldo >= 0, moeda, versão e o cenário
// obrigatório do §8 (100.00 contra duas apostas de 80.00).
package wallet

import (
	"errors"
	"testing"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
)

// fixedTime: instante controlado (UTC, como o agregado normaliza) para resultados determinísticos.
var fixedTime = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

// mustMoney centraliza a construção de Money nos testes.
func mustMoney(t *testing.T, amount string, currency money.Currency) money.Money {
	t.Helper()
	m, err := money.Parse(amount, currency)
	if err != nil {
		t.Fatalf("money.Parse(%q, %s) unexpected error: %v", amount, currency, err)
	}
	return m
}

// mustNewWallet cria uma carteira válida para os testes de comportamento.
func mustNewWallet(t *testing.T, initialBalance money.Money) *Wallet {
	t.Helper()
	id, err := NewID("0192f291-27dd-7d3f-8071-5f8685deef37")
	if err != nil {
		t.Fatalf("NewID unexpected error: %v", err)
	}
	playerID, err := NewPlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	if err != nil {
		t.Fatalf("NewPlayerID unexpected error: %v", err)
	}
	w, err := New(id, playerID, initialBalance, fixedTime)
	if err != nil {
		t.Fatalf("New unexpected error: %v", err)
	}
	return w
}

// TestNew_Valid cobre a criação com saldo zero e com saldo positivo.
func TestNew_Valid(t *testing.T) {
	cases := []struct {
		name          string
		initialAmount string
		wantVersion   int64
		wantBalance   string
	}{
		// §9: saldo inicial zero é aceito; a carteira nasce em versão 1 e o
		// caso de uso NÃO cria transação OPENING nem eventos financeiros.
		{name: "saldo_inicial_zero", initialAmount: "0.00", wantVersion: 1, wantBalance: "0.00"},
		{name: "saldo_inicial_positivo", initialAmount: "1000.00", wantVersion: 1, wantBalance: "1000.00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := mustNewWallet(t, mustMoney(t, tc.initialAmount, money.BRL))

			if got := w.Balance().String(); got != tc.wantBalance {
				t.Errorf("Balance() = %q, want %q", got, tc.wantBalance)
			}
			// §9: "a versão da carteira nessa abertura é 1" — mesmo com crédito.
			if got := w.Version(); got != tc.wantVersion {
				t.Errorf("Version() = %d, want %d", got, tc.wantVersion)
			}
			if got := w.Currency(); got != money.BRL {
				t.Errorf("Currency() = %q, want %q", got, money.BRL)
			}
			if !w.CreatedAt().Equal(fixedTime) || !w.UpdatedAt().Equal(fixedTime) {
				t.Errorf("timestamps = (%v, %v), want %v", w.CreatedAt(), w.UpdatedAt(), fixedTime)
			}
			// A moeda da carteira vem do saldo inicial — não existe parâmetro
			// separado que pudesse divergir.
			if got := w.Balance().Currency(); got != w.Currency() {
				t.Errorf("moeda do saldo %q difere da moeda da carteira %q", got, w.Currency())
			}
		})
	}
}

// TestNew_Invalid cobre todas as rejeições de criação.
func TestNew_Invalid(t *testing.T) {
	validID, _ := NewID("0192f291-27dd-7d3f-8071-5f8685deef37")
	validPlayer, _ := NewPlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	zeroMoney, _ := money.Zero(money.BRL)

	t.Run("id_vazio", func(t *testing.T) {
		if _, err := New("", validPlayer, zeroMoney, fixedTime); !errors.Is(err, ErrInvalidID) {
			t.Errorf("error = %v, want ErrInvalidID", err)
		}
	})

	t.Run("player_vazio", func(t *testing.T) {
		if _, err := New(validID, "", zeroMoney, fixedTime); !errors.Is(err, ErrInvalidPlayer) {
			t.Errorf("error = %v, want ErrInvalidPlayer", err)
		}
	})

	t.Run("timestamp_zero", func(t *testing.T) {
		if _, err := New(validID, validPlayer, zeroMoney, time.Time{}); !errors.Is(err, ErrInvalidTimestamp) {
			t.Errorf("error = %v, want ErrInvalidTimestamp", err)
		}
	})

	t.Run("saldo_sem_moeda", func(t *testing.T) {
		if _, err := New(validID, validPlayer, money.Money{}, fixedTime); !errors.Is(err, money.ErrUninitialized) {
			t.Errorf("error = %v, want money.ErrUninitialized", err)
		}
	})

	t.Run("saldo_inicial_negativo", func(t *testing.T) {
		// money.New aceita negativos (uso interno), mas a ABERTURA de carteira não.
		negative, err := money.New(-100, money.BRL)
		if err != nil {
			t.Fatalf("money.New unexpected error: %v", err)
		}
		if _, err := New(validID, validPlayer, negative, fixedTime); !errors.Is(err, ErrNegativeInitialBalance) {
			t.Errorf("error = %v, want ErrNegativeInitialBalance", err)
		}
	})
}

// TestRehydrate prova que a reidratação NÃO reaplica movimentações (§6).
func TestRehydrate(t *testing.T) {
	validID, _ := NewID("0192f291-27dd-7d3f-8071-5f8685deef37")
	validPlayer, _ := NewPlayerID("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	balance := mustMoney(t, "975.00", money.BRL)

	t.Run("preserva_estado_sem_movimentar", func(t *testing.T) {
		later := fixedTime.Add(2 * time.Hour)

		w, err := Rehydrate(validID, validPlayer, balance, 3, fixedTime, later)
		if err != nil {
			t.Fatalf("Rehydrate unexpected error: %v", err)
		}

		// O estado deve ser EXATAMENTE o informado: nada de reaplicar créditos.
		if got := w.Balance().String(); got != "975.00" {
			t.Errorf("Balance() = %q, want %q", got, "975.00")
		}
		if got := w.Version(); got != 3 {
			t.Errorf("Version() = %d, want 3", got)
		}
		// A versão não deve ter mudado por causa da leitura.
		if got := w.UpdatedAt(); !got.Equal(later) {
			t.Errorf("UpdatedAt() = %v, want %v", got, later)
		}
	})

	t.Run("saldo_negativo_corrompido", func(t *testing.T) {
		negative, err := money.New(-1, money.BRL)
		if err != nil {
			t.Fatalf("money.New unexpected error: %v", err)
		}
		if _, err := Rehydrate(validID, validPlayer, negative, 1, fixedTime, fixedTime); !errors.Is(err, ErrInvalidBalance) {
			t.Errorf("error = %v, want ErrInvalidBalance", err)
		}
	})

	t.Run("versao_invalida", func(t *testing.T) {
		if _, err := Rehydrate(validID, validPlayer, balance, 0, fixedTime, fixedTime); !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("error = %v, want ErrInvalidVersion", err)
		}
	})

	t.Run("timestamps_ausentes", func(t *testing.T) {
		if _, err := Rehydrate(validID, validPlayer, balance, 1, time.Time{}, fixedTime); !errors.Is(err, ErrInvalidTimestamp) {
			t.Errorf("error = %v, want ErrInvalidTimestamp", err)
		}
	})
}

// TestDebit cobre o caminho feliz e as rejeições do débito.
func TestDebit(t *testing.T) {
	t.Run("sucesso_incrementa_versao", func(t *testing.T) {
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		movement, err := w.Debit(mustMoney(t, "80.00", money.BRL), fixedTime.Add(time.Minute))
		if err != nil {
			t.Fatalf("Debit unexpected error: %v", err)
		}

		if got := w.Balance().String(); got != "20.00" {
			t.Errorf("Balance() = %q, want %q", got, "20.00")
		}
		if got := w.Version(); got != 2 {
			t.Errorf("Version() = %d, want 2", got)
		}
		// O Movement descreve exatamente o que ocorreu — é o que alimenta o
		// ledger (balanceBefore/After) e o evento WalletBalanceChanged.
		if movement.Direction() != Debit {
			t.Errorf("Direction() = %q, want %q", movement.Direction(), Debit)
		}
		if got := movement.BalanceBefore().String(); got != "100.00" {
			t.Errorf("BalanceBefore() = %q, want %q", got, "100.00")
		}
		if got := movement.BalanceAfter().String(); got != "20.00" {
			t.Errorf("BalanceAfter() = %q, want %q", got, "20.00")
		}
		if got := movement.Amount().String(); got != "80.00" {
			t.Errorf("Amount() = %q, want %q", got, "80.00")
		}
		if got := movement.WalletVersion(); got != 2 {
			t.Errorf("WalletVersion() = %d, want 2", got)
		}
	})

	t.Run("saldo_extato_zera_permitido", func(t *testing.T) {
		// Débito que leva o saldo a EXATAMENTE zero é permitido: a regra é
		// "saldo >= 0", não "> 0" (§6.2).
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		if _, err := w.Debit(mustMoney(t, "100.00", money.BRL), fixedTime); err != nil {
			t.Fatalf("Debit unexpected error: %v", err)
		}
		if !w.Balance().IsZero() {
			t.Errorf("Balance() = %s, want 0.00", w.Balance())
		}
	})

	t.Run("saldo_insuficiente_nao_altera_nada", func(t *testing.T) {
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		_, err := w.Debit(mustMoney(t, "100.01", money.BRL), fixedTime)
		if !errors.Is(err, ErrInsufficientFunds) {
			t.Fatalf("error = %v, want ErrInsufficientFunds", err)
		}
		// Fundamental: em caso de rejeição, o agregado fica INTACTO.
		if got := w.Balance().String(); got != "100.00" {
			t.Errorf("Balance() = %q, want %q (agregado deve ficar intacto)", got, "100.00")
		}
		if got := w.Version(); got != 1 {
			t.Errorf("Version() = %d, want 1 (sem mudança de saldo)", got)
		}
	})

	t.Run("valor_zero_rejeitado", func(t *testing.T) {
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		// §7: LOSS tem valor zero e NÃO movimenta a carteira; portanto zero
		// nunca deve chegar a Debit/Credit.
		if _, err := w.Debit(mustMoney(t, "0.00", money.BRL), fixedTime); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("error = %v, want ErrNonPositiveAmount", err)
		}
	})

	t.Run("valor_negativo_rejeitado", func(t *testing.T) {
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		negative, err := money.New(-100, money.BRL)
		if err != nil {
			t.Fatalf("money.New unexpected error: %v", err)
		}
		if _, err := w.Debit(negative, fixedTime); !errors.Is(err, ErrNonPositiveAmount) {
			t.Errorf("error = %v, want ErrNonPositiveAmount", err)
		}
	})

	t.Run("moeda_incompativel", func(t *testing.T) {
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		// §6.2: a moeda da movimentação deve coincidir com a da carteira.
		if _, err := w.Debit(mustMoney(t, "10.00", money.USD), fixedTime); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Errorf("error = %v, want money.ErrCurrencyMismatch", err)
		}
	})

	t.Run("timestamp_zero_rejeitado", func(t *testing.T) {
		w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

		if _, err := w.Debit(mustMoney(t, "10.00", money.BRL), time.Time{}); !errors.Is(err, ErrInvalidTimestamp) {
			t.Errorf("error = %v, want ErrInvalidTimestamp", err)
		}
	})
}

// TestCredit cobre o caminho feliz do crédito.
func TestCredit(t *testing.T) {
	w := mustNewWallet(t, mustMoney(t, "25.00", money.BRL))

	movement, err := w.Credit(mustMoney(t, "15.50", money.BRL), fixedTime.Add(time.Minute))
	if err != nil {
		t.Fatalf("Credit unexpected error: %v", err)
	}

	if got := w.Balance().String(); got != "40.50" {
		t.Errorf("Balance() = %q, want %q", got, "40.50")
	}
	if got := w.Version(); got != 2 {
		t.Errorf("Version() = %d, want 2", got)
	}
	if movement.Direction() != Credit {
		t.Errorf("Direction() = %q, want %q", movement.Direction(), Credit)
	}
}

// TestNilReceiver: Debit/Credit em *Wallet nil devolvem erro tipado, não panic.
func TestNilReceiver(t *testing.T) {
	var w *Wallet

	if _, err := w.Debit(mustMoney(t, "10.00", money.BRL), fixedTime); !errors.Is(err, ErrUninitialized) {
		t.Errorf("error = %v, want ErrUninitialized", err)
	}
	if _, err := w.Credit(mustMoney(t, "10.00", money.BRL), fixedTime); !errors.Is(err, ErrUninitialized) {
		t.Errorf("error = %v, want ErrUninitialized", err)
	}
}

// TestBalanceNotAliased: o Money devolvido por Balance() não é alias mutável do estado.
func TestBalanceNotAliased(t *testing.T) {
	w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

	exposed := w.Balance()
	// Operações produzem novas instâncias; o saldo da carteira permanece o mesmo.
	_, err := exposed.Add(mustMoney(t, "50.00", money.BRL))
	if err != nil {
		t.Fatalf("Add unexpected error: %v", err)
	}

	if got := w.Balance().String(); got != "100.00" {
		t.Errorf("Balance() = %q, want %q (o agregado não pode ser afetado)", got, "100.00")
	}
}

// TestDomainRaceScenario: cenário do §8 (100.00, duas apostas de 80.00) em nível de
// domínio — a primeira aceita, a segunda rejeitada, saldo 20.00. Não é o teste de
// concorrência real (que exige múltiplos processos); aqui só provisionamos a regra.
func TestDomainRaceScenario(t *testing.T) {
	w := mustNewWallet(t, mustMoney(t, "100.00", money.BRL))

	first, err := w.Debit(mustMoney(t, "80.00", money.BRL), fixedTime)
	if err != nil {
		t.Fatalf("primeira aposta deveria ser aceita: %v", err)
	}

	_, err = w.Debit(mustMoney(t, "80.00", money.BRL), fixedTime)
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("segunda aposta deveria ser rejeitada por saldo insuficiente, got %v", err)
	}

	if got := w.Balance().String(); got != "20.00" {
		t.Errorf("saldo final = %q, want %q", got, "20.00")
	}
	// Exatamente UMA movimentação confirmada: 2 versões (1 na criação + 1 débito).
	if got := w.Version(); got != 2 {
		t.Errorf("Version() = %d, want 2 (uma unica mudanca de saldo)", got)
	}
	if got := first.BalanceAfter().String(); got != "20.00" {
		t.Errorf("BalanceAfter() da aposta aceita = %q, want %q", got, "20.00")
	}
}

// TestIsBlank cobre o helper de validação estrutural (white-box testing).
func TestIsBlank(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "vazio", input: "", want: true},
		{name: "apenas_espacos", input: "   ", want: true},
		{name: "tabs_e_quebras", input: " \t\r\n ", want: true},
		{name: "com_conteudo", input: "id-123", want: false},
		{name: "espacos_nas_bordas", input: " id ", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBlank(tc.input); got != tc.want {
				t.Errorf("isBlank(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
