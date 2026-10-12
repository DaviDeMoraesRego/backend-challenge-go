// Testes white-box do Value Object Money (§13): parsing estrito, escala, overflow,
// entradas inválidas, incompatibilidade de moedas, aritmética e JSON. O pacote é
// `money` (não `money_test`) para acessar os helpers internos de overflow.
package money

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

// mustParse falha imediatamente se o parsing não funcionar. t.Helper() faz o relatório
// apontar o chamador, não a linha interna do helper.
func mustParse(t *testing.T, amount string, currency Currency) Money {
	t.Helper()
	m, err := Parse(amount, currency)
	if err != nil {
		t.Fatalf("Parse(%q, %s) unexpected error: %v", amount, currency, err)
	}
	return m
}

// TestParse_Valid cobre as formas ACEITAS de entrada decimal e a normalização
// para unidades mínimas.
func TestParse_Valid(t *testing.T) {
	// Cada campo documenta o comportamento esperado. `wantMinor` é o valor
	// exato em centavos — nunca comparamos float aqui (§5.1).
	cases := []struct {
		name      string
		amount    string
		wantMinor int64
		wantStr   string // forma canônica após String()
	}{
		{name: "duas casas", amount: "25.00", wantMinor: 2500, wantStr: "25.00"},
		{name: "sem casas decimais", amount: "25", wantMinor: 2500, wantStr: "25.00"},
		{name: "uma casa decimal", amount: "25.5", wantMinor: 2550, wantStr: "25.50"},
		{name: "zero com casas", amount: "0.00", wantMinor: 0, wantStr: "0.00"},
		{name: "zero sem casas", amount: "0", wantMinor: 0, wantStr: "0.00"},
		{name: "centavos", amount: "0.05", wantMinor: 5, wantStr: "0.05"},
		{name: "meio real", amount: "0.5", wantMinor: 50, wantStr: "0.50"},
		{name: "zeros a esquerda", amount: "007.00", wantMinor: 700, wantStr: "7.00"},
		{name: "mil reais", amount: "1000.00", wantMinor: 100000, wantStr: "1000.00"},
		{
			// Valor máximo representável: MaxInt64 == 9223372036854775807 centavos.
			// Este caso prova que o parsing NÃO estoura no limite exato.
			name: "maximo representavel", amount: "92233720368547758.07",
			wantMinor: math.MaxInt64, wantStr: "92233720368547758.07",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := mustParse(t, tc.amount, BRL)

			if got := m.AmountMinor(); got != tc.wantMinor {
				t.Errorf("AmountMinor() = %d, want %d", got, tc.wantMinor)
			}
			if got := m.String(); got != tc.wantStr {
				t.Errorf("String() = %q, want %q", got, tc.wantStr)
			}
			if got := m.Currency(); got != BRL {
				t.Errorf("Currency() = %q, want %q", got, BRL)
			}
			// Round-trip: String() -> Parse() deve ser idempotente.
			reparsed := mustParse(t, m.String(), BRL)
			if !reparsed.Equal(m) {
				t.Errorf("round-trip divergiu: %s != %s", reparsed, m)
			}
		})
	}
}

// TestParse_Invalid: toda entrada ambígua é rejeitada, nunca normalizada em silêncio.
func TestParse_Invalid(t *testing.T) {
	cases := []struct {
		name   string
		amount string
		want   error // sentinela esperado
	}{
		{name: "vazio", amount: "", want: ErrInvalidAmount},
		{name: "apenas espacos", amount: "   ", want: ErrInvalidAmount},
		{name: "espaco no meio", amount: "25 00", want: ErrInvalidAmount},
		{name: "espaco a esquerda", amount: " 25.00", want: ErrInvalidAmount},
		{name: "espaco a direita", amount: "25.00 ", want: ErrInvalidAmount},
		{name: "sem parte inteira", amount: ".50", want: ErrInvalidAmount},
		{name: "sem parte fracionaria", amount: "25.", want: ErrInvalidAmount},
		{name: "escala excedente", amount: "25.005", want: ErrInvalidAmount},
		{name: "escala excedente longa", amount: "25.0000001", want: ErrInvalidAmount},
		{name: "negativo", amount: "-25.00", want: ErrInvalidAmount},
		{name: "sinal positivo", amount: "+25.00", want: ErrInvalidAmount},
		{name: "notacao cientifica", amount: "1e2", want: ErrInvalidAmount},
		{name: "notacao cientifica maiuscula", amount: "1E2", want: ErrInvalidAmount},
		{name: "NaN", amount: "NaN", want: ErrInvalidAmount},
		{name: "infinito", amount: "Inf", want: ErrInvalidAmount},
		{name: "infinito negativo", amount: "-Inf", want: ErrInvalidAmount},
		{name: "virgula decimal", amount: "25,00", want: ErrInvalidAmount},
		{name: "dois separadores", amount: "2.5.0", want: ErrInvalidAmount},
		{name: "texto", amount: "abc", want: ErrInvalidAmount},
		{name: "hexadecimal", amount: "0x10", want: ErrInvalidAmount},
		{name: "underscore", amount: "1_000.00", want: ErrInvalidAmount},
		// Overflow: 92233720368547758.08 * 100 == 2^63, exatamente 1 acima de MaxInt64.
		{name: "overflow no limite", amount: "92233720368547758.08", want: ErrOverflow},
		{name: "overflow parte inteira", amount: "99999999999999999999", want: ErrOverflow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tc.amount, BRL)
			if err == nil {
				t.Fatalf("Parse(%q) expected error, got nil", tc.amount)
			}
			// errors.Is percorre a cadeia de wrapping; comparar texto seria frágil.
			if !errors.Is(err, tc.want) {
				t.Errorf("Parse(%q) error = %v, want sentinel %v", tc.amount, err, tc.want)
			}
		})
	}
}

// TestParse_InvalidCurrency garante que a moeda é validada junto com o valor.
func TestParse_InvalidCurrency(t *testing.T) {
	// Conversão direta simula um caller que burlou o construtor.
	invalidCurrency := Currency("brl")

	_, err := Parse("25.00", invalidCurrency)
	if !errors.Is(err, ErrInvalidCurrency) {
		t.Fatalf("error = %v, want ErrInvalidCurrency", err)
	}
}

// TestNewCurrency valida o Value Object de moeda isoladamente.
func TestNewCurrency(t *testing.T) {
	valid := []string{"BRL", "USD", "EUR", "XXX"}
	for _, code := range valid {
		t.Run("valido_"+code, func(t *testing.T) {
			got, err := NewCurrency(code)
			if err != nil {
				t.Fatalf("NewCurrency(%q) unexpected error: %v", code, err)
			}
			if got.String() != code {
				t.Errorf("String() = %q, want %q", got.String(), code)
			}
		})
	}

	invalid := []string{"", "brl", "Brl", "BR", "BRLX", "B1L", "BRL ", " BRL", "R$", "123"}
	for _, code := range invalid {
		t.Run("invalido_"+code, func(t *testing.T) {
			if _, err := NewCurrency(code); !errors.Is(err, ErrInvalidCurrency) {
				t.Errorf("NewCurrency(%q) error = %v, want ErrInvalidCurrency", code, err)
			}
		})
	}
}

// TestZero prova o elemento neutro por moeda (§6.1: "zero por moeda").
func TestZero(t *testing.T) {
	zero, err := Zero(BRL)
	if err != nil {
		t.Fatalf("Zero(BRL) unexpected error: %v", err)
	}
	if !zero.IsZero() {
		t.Errorf("Zero(BRL).IsZero() = false, want true")
	}
	if zero.String() != "0.00" {
		t.Errorf("Zero(BRL).String() = %q, want %q", zero.String(), "0.00")
	}
	if zero.Currency() != BRL {
		t.Errorf("Zero(BRL).Currency() = %q, want %q", zero.Currency(), BRL)
	}
}

// TestZeroValueRejected: valores não inicializados são rejeitados em runtime (§6).
func TestZeroValueRejected(t *testing.T) {
	var uninitialized Money

	operations := map[string]func() error{
		"Add":     func() error { _, err := uninitialized.Add(uninitialized); return err },
		"Sub":     func() error { _, err := uninitialized.Sub(uninitialized); return err },
		"Negate":  func() error { _, err := uninitialized.Negate(); return err },
		"Compare": func() error { _, err := uninitialized.Compare(uninitialized); return err },
		"Marshal": func() error { _, err := json.Marshal(uninitialized); return err },
	}

	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, ErrUninitialized) {
				t.Errorf("error = %v, want ErrUninitialized", err)
			}
		})
	}
}

// TestAdd cobre soma com sucesso e o overflow no limite do int64.
func TestAdd(t *testing.T) {
	t.Run("soma_simples", func(t *testing.T) {
		a := mustParse(t, "25.00", BRL)
		b := mustParse(t, "10.50", BRL)

		sum, err := a.Add(b)
		if err != nil {
			t.Fatalf("Add unexpected error: %v", err)
		}
		if sum.String() != "35.50" {
			t.Errorf("sum = %q, want %q", sum.String(), "35.50")
		}
		// IMUTABILIDADE: os operandos NÃO podem ter mudado.
		if a.String() != "25.00" || b.String() != "10.50" {
			t.Errorf("operandos foram mutados: a=%q b=%q", a.String(), b.String())
		}
	})

	t.Run("overflow", func(t *testing.T) {
		maxMoney, err := New(math.MaxInt64, BRL)
		if err != nil {
			t.Fatalf("New unexpected error: %v", err)
		}
		one, err := New(1, BRL)
		if err != nil {
			t.Fatalf("New unexpected error: %v", err)
		}

		if _, err := maxMoney.Add(one); !errors.Is(err, ErrOverflow) {
			t.Errorf("Add at MaxInt64 error = %v, want ErrOverflow", err)
		}
	})

	t.Run("moeda_incompativel", func(t *testing.T) {
		brl := mustParse(t, "10.00", BRL)
		usd := mustParse(t, "10.00", USD)

		if _, err := brl.Add(usd); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("Add cross-currency error = %v, want ErrCurrencyMismatch", err)
		}
	})
}

// TestSub cobre subtração (incluindo resultado negativo interno) e overflow.
func TestSub(t *testing.T) {
	t.Run("resultado_negativo_permitido", func(t *testing.T) {
		// Negativo é VÁLIDO em diferenças internas (§6.1); quem barra saldo
		// negativo é o agregado Wallet, não o Value Object.
		a := mustParse(t, "10.00", BRL)
		b := mustParse(t, "25.00", BRL)

		diff, err := a.Sub(b)
		if err != nil {
			t.Fatalf("Sub unexpected error: %v", err)
		}
		if diff.AmountMinor() != -1500 {
			t.Errorf("AmountMinor() = %d, want -1500", diff.AmountMinor())
		}
		if diff.String() != "-15.00" {
			t.Errorf("String() = %q, want %q", diff.String(), "-15.00")
		}
		if !diff.IsNegative() {
			t.Errorf("IsNegative() = false, want true")
		}
	})

	t.Run("overflow", func(t *testing.T) {
		minMoney, err := New(math.MinInt64, BRL)
		if err != nil {
			t.Fatalf("New unexpected error: %v", err)
		}
		one, err := New(1, BRL)
		if err != nil {
			t.Fatalf("New unexpected error: %v", err)
		}

		if _, err := minMoney.Sub(one); !errors.Is(err, ErrOverflow) {
			t.Errorf("Sub at MinInt64 error = %v, want ErrOverflow", err)
		}
	})
}

// TestNegate cobre a inversão de sinal e o caso não representável MinInt64.
func TestNegate(t *testing.T) {
	t.Run("simples", func(t *testing.T) {
		positive := mustParse(t, "25.00", BRL)
		negative, err := positive.Negate()
		if err != nil {
			t.Fatalf("Negate unexpected error: %v", err)
		}
		if negative.String() != "-25.00" {
			t.Errorf("negated = %q, want %q", negative.String(), "-25.00")
		}
	})

	t.Run("min_int64_overflow", func(t *testing.T) {
		// O oposto de -2^63 não é representável: precisa falhar, não fazer wrap.
		minMoney, err := New(math.MinInt64, BRL)
		if err != nil {
			t.Fatalf("New unexpected error: %v", err)
		}
		if _, err := minMoney.Negate(); !errors.Is(err, ErrOverflow) {
			t.Errorf("Negate(MinInt64) error = %v, want ErrOverflow", err)
		}
	})
}

// TestString_MinInt64: a formatação do absoluto não estoura (absUint64 usa (-(a+1))+1).
func TestString_MinInt64(t *testing.T) {
	minMoney, err := New(math.MinInt64, BRL)
	if err != nil {
		t.Fatalf("New unexpected error: %v", err)
	}
	if got, want := minMoney.String(), "-92233720368547758.08"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestCompare cobre comparação e mismatch de moeda.
func TestCompare(t *testing.T) {
	cases := []struct {
		name string
		a    string
		b    string
		want int
	}{
		{name: "menor", a: "10.00", b: "25.00", want: -1},
		{name: "maior", a: "25.00", b: "10.00", want: 1},
		{name: "igual", a: "25.00", b: "25.00", want: 0},
		{name: "normalizacao_equivalente", a: "25.5", b: "25.50", want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := mustParse(t, tc.a, BRL)
			b := mustParse(t, tc.b, BRL)

			got, err := a.Compare(b)
			if err != nil {
				t.Fatalf("Compare unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("Compare = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("moeda_incompativel", func(t *testing.T) {
		brl := mustParse(t, "10.00", BRL)
		usd := mustParse(t, "10.00", USD)

		if _, err := brl.Compare(usd); !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("Compare cross-currency error = %v, want ErrCurrencyMismatch", err)
		}
	})
}

// TestEqual: igualdade exige mesma moeda e valor. Diferente de Compare, moedas
// distintas aqui não são erro — são apenas diferentes.
func TestEqual(t *testing.T) {
	if !mustParse(t, "25.50", BRL).Equal(mustParse(t, "25.5", BRL)) {
		t.Errorf("valores equivalentes deveriam ser iguais")
	}
	if mustParse(t, "25.50", BRL).Equal(mustParse(t, "25.50", USD)) {
		t.Errorf("moedas diferentes não podem ser iguais")
	}
	if mustParse(t, "1.00", BRL).Equal(mustParse(t, "2.00", BRL)) {
		t.Errorf("valores diferentes não podem ser iguais")
	}
}

// TestJSON_RoundTrip prova o contrato externo {"amount":"...","currency":"..."}.
func TestJSON_RoundTrip(t *testing.T) {
	original := mustParse(t, "25.00", BRL)

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal unexpected error: %v", err)
	}

	// Comparação do JSON canônico: campos em ordem determinística.
	wantJSON := `{"amount":"25.00","currency":"BRL"}`
	if string(encoded) != wantJSON {
		t.Errorf("Marshal = %s, want %s", encoded, wantJSON)
	}

	var decoded Money
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal unexpected error: %v", err)
	}
	if !decoded.Equal(original) {
		t.Errorf("round-trip divergiu: %s != %s", decoded, original)
	}
}

// TestJSON_Strict prova que o parsing de JSON recusa formatos perigosos.
func TestJSON_Strict(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    error
	}{
		{
			name:    "amount_numerico_proibido",
			payload: `{"amount":25.00,"currency":"BRL"}`,
			// Um número JSON viraria float64 no consumidor: proibido (§5.1).
			want: ErrInvalidAmount,
		},
		{
			name:    "campo_desconhecido",
			payload: `{"amount":"25.00","currency":"BRL","extra":1}`,
			want:    ErrInvalidAmount,
		},
		{
			name:    "moeda_invalida",
			payload: `{"amount":"25.00","currency":"brl"}`,
			want:    ErrInvalidCurrency,
		},
		{
			name:    "escala_excedente",
			payload: `{"amount":"25.005","currency":"BRL"}`,
			want:    ErrInvalidAmount,
		},
		{
			name:    "objeto_vazio",
			payload: `{}`,
			want:    ErrInvalidCurrency,
		},
		{
			name:    "tipo_errado",
			payload: `"25.00"`,
			want:    ErrInvalidAmount,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m Money
			err := json.Unmarshal([]byte(tc.payload), &m)
			if err == nil {
				t.Fatalf("Unmarshal(%s) expected error, got nil", tc.payload)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Unmarshal(%s) error = %v, want sentinel %v", tc.payload, err, tc.want)
			}
		})
	}
}

// TestOverflowHelpers testa os helpers internos de overflow (só possível em white-box).
func TestOverflowHelpers(t *testing.T) {
	cases := []struct {
		name   string
		run    func() bool // retorna o "ok" do helper
		wantOK bool
	}{
		{name: "add_sem_overflow", run: func() bool { _, ok := addChecked(1, 2); return ok }, wantOK: true},
		{name: "add_overflow_positivo", run: func() bool { _, ok := addChecked(math.MaxInt64, 1); return ok }, wantOK: false},
		{name: "add_overflow_negativo", run: func() bool { _, ok := addChecked(math.MinInt64, -1); return ok }, wantOK: false},
		{name: "sub_sem_overflow", run: func() bool { _, ok := subChecked(5, 3); return ok }, wantOK: true},
		{name: "sub_overflow", run: func() bool { _, ok := subChecked(math.MinInt64, 1); return ok }, wantOK: false},
		{name: "mul_sem_overflow", run: func() bool { _, ok := mulChecked(100, 100); return ok }, wantOK: true},
		{name: "mul_overflow", run: func() bool { _, ok := mulChecked(math.MaxInt64, 2); return ok }, wantOK: false},
		{name: "mul_min_por_menos_um", run: func() bool { _, ok := mulChecked(math.MinInt64, -1); return ok }, wantOK: false},
		{name: "neg_min_int64", run: func() bool { _, ok := negChecked(math.MinInt64); return ok }, wantOK: false},
		{name: "neg_comum", run: func() bool { _, ok := negChecked(42); return ok }, wantOK: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run(); got != tc.wantOK {
				t.Errorf("ok = %v, want %v", got, tc.wantOK)
			}
		})
	}
}
