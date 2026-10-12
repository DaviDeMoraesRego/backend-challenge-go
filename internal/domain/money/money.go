package money

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

var (
	// ErrInvalidAmount: string não é decimal válido dentro das regras.
	ErrInvalidAmount = errors.New("money: invalid amount")

	// ErrCurrencyMismatch: §6.1 — aritmética e comparação exigem moedas iguais.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")

	// ErrOverflow: resultado não cabe em int64 (parsing, soma, subtração, negação).
	ErrOverflow = errors.New("money: overflow")

	// ErrUninitialized: valor não passou pelo construtor (§6).
	ErrUninitialized = errors.New("money: uninitialized value")
)

// Money é o Value Object imutável: unidades mínimas (centavos) + moeda.
// int64 cobre -2^63 a 2^63-1 centavos; fora disso, ErrOverflow. Nunca float (§5.1).
type Money struct {
	amountMinor int64
	currency    Currency
}

// New constrói um Money a partir de unidades mínimas (centavos).
// Aceita negativos (válidos em diferenças internas, §6.1); Parse é a fronteira externa.
func New(amountMinor int64, currency Currency) (Money, error) {
	// Construtores nunca confiam no tipo: `Currency("brl")` burlaria a validação.
	if _, err := NewCurrency(currency.String()); err != nil {
		return Money{}, err
	}
	return Money{amountMinor: amountMinor, currency: currency}, nil
}

// Zero devolve o neutro aditivo de uma moeda (`Money{}` não tem moeda).
func Zero(currency Currency) (Money, error) {
	return New(0, currency)
}

// Parse converte uma string decimal da fronteira externa para Money.
//
// Regras de aceitação (definem o hash de idempotência §9): parte inteira obrigatória,
// no máximo um ponto, até 2 casas decimais, sem sinal, sem notação científica e sem
// espaços. "25" e "25.5" normalizam para 2500 e 2550 centavos — a normalização é
// determinística e ocorre ANTES do hash, então valores equivalentes produzem o mesmo.
func Parse(amount string, currency Currency) (Money, error) {
	cur, err := NewCurrency(currency.String())
	if err != nil {
		return Money{}, err
	}
	minor, err := parseMinorUnits(amount)
	if err != nil {
		return Money{}, err
	}
	return Money{amountMinor: minor, currency: cur}, nil
}

// parseMinorUnits é a rotina interna de parsing (valida o valor; a moeda é validada por Parse).
func parseMinorUnits(amount string) (int64, error) {
	if amount == "" {
		return 0, fmt.Errorf("%w: empty string", ErrInvalidAmount)
	}
	if strings.ContainsAny(amount, " \t\r\n") {
		return 0, fmt.Errorf("%w: %q must not contain whitespace", ErrInvalidAmount, amount)
	}
	if strings.Count(amount, ".") > 1 {
		return 0, fmt.Errorf("%w: %q has more than one decimal separator", ErrInvalidAmount, amount)
	}
	integerPart := amount
	fractionPart := ""
	hasDot := strings.IndexByte(amount, '.') >= 0
	if hasDot {
		dot := strings.IndexByte(amount, '.')
		integerPart = amount[:dot]
		fractionPart = amount[dot+1:]
	}
	if integerPart == "" {
		return 0, fmt.Errorf("%w: %q is missing the integer part", ErrInvalidAmount, amount)
	}
	if hasDot && fractionPart == "" {
		return 0, fmt.Errorf("%w: %q is missing the fractional part", ErrInvalidAmount, amount)
	}
	// Escala máxima de 2 casas (§6.1): impede perda silenciosa.
	if len(fractionPart) > 2 {
		return 0, fmt.Errorf("%w: %q has more than 2 decimal places", ErrInvalidAmount, amount)
	}
	// Este laço rejeita '-', '+', 'e'/'E', 'N'/'I' e qualquer não-dígito.
	for i := 0; i < len(integerPart); i++ {
		if integerPart[i] < '0' || integerPart[i] > '9' {
			return 0, fmt.Errorf("%w: %q contains invalid character %q", ErrInvalidAmount, amount, integerPart[i])
		}
	}
	for i := 0; i < len(fractionPart); i++ {
		if fractionPart[i] < '0' || fractionPart[i] > '9' {
			return 0, fmt.Errorf("%w: %q contains invalid character %q", ErrInvalidAmount, amount, fractionPart[i])
		}
	}

	// Acumula a parte inteira com checagem de overflow a cada dígito.
	var whole int64
	for i := 0; i < len(integerPart); i++ {
		digit := int64(integerPart[i] - '0')
		// whole*10+digit > MaxInt64 reescrito como divisão para evitar o overflow.
		if whole > (math.MaxInt64-digit)/10 {
			return 0, fmt.Errorf("%w: %q exceeds maximum representable amount", ErrOverflow, amount)
		}
		whole = whole*10 + digit
	}
	wholeInCents, ok := mulChecked(whole, 100)
	if !ok {
		return 0, fmt.Errorf("%w: %q exceeds maximum representable amount", ErrOverflow, amount)
	}
	// "5" fracionário significa 50 centavos: padroniza à direita até 2 dígitos.
	for len(fractionPart) < 2 {
		fractionPart += "0"
	}
	var cents int64
	for i := 0; i < len(fractionPart); i++ {
		cents = cents*10 + int64(fractionPart[i]-'0')
	}
	total, ok := addChecked(wholeInCents, cents)
	if !ok {
		return 0, fmt.Errorf("%w: %q exceeds maximum representable amount", ErrOverflow, amount)
	}
	return total, nil
}

// AmountMinor é o valor persistido em BIGINT e usado nos cálculos internos (§5.1).
func (m Money) AmountMinor() int64 { return m.amountMinor }

func (m Money) Currency() Currency { return m.currency }

// validate rejeita o zero value `Money{}` (valor de domínio não inicializado, §6).
func (m Money) validate() error {
	if m.currency.IsZero() {
		return ErrUninitialized
	}
	return nil
}

// Add soma dois valores da mesma moeda. Dinheiro não representável é condição de
// negócio, não bug: retorna erro em vez de panic (o spec proíbe panic em rejeição).
func (m Money) Add(other Money) (Money, error) {
	if err := m.validate(); err != nil {
		return Money{}, err
	}
	if err := other.validate(); err != nil {
		return Money{}, err
	}
	if !m.currency.Equal(other.currency) {
		return Money{}, fmt.Errorf("%w: cannot add %s to %s", ErrCurrencyMismatch, other.currency, m.currency)
	}
	sum, ok := addChecked(m.amountMinor, other.amountMinor)
	if !ok {
		return Money{}, fmt.Errorf("%w: adding %s to %s", ErrOverflow, other, m)
	}
	return Money{amountMinor: sum, currency: m.currency}, nil
}

// Sub subtrai `other` de `m`. O resultado pode ser negativo (diferenças internas
// são válidas); barrar saldo negativo é papel do agregado Wallet e do banco.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.validate(); err != nil {
		return Money{}, err
	}
	if err := other.validate(); err != nil {
		return Money{}, err
	}
	if !m.currency.Equal(other.currency) {
		return Money{}, fmt.Errorf("%w: cannot subtract %s from %s", ErrCurrencyMismatch, other.currency, m.currency)
	}
	diff, ok := subChecked(m.amountMinor, other.amountMinor)
	if !ok {
		return Money{}, fmt.Errorf("%w: subtracting %s from %s", ErrOverflow, other, m)
	}
	return Money{amountMinor: diff, currency: m.currency}, nil
}

// Negate inverte o sinal. MinInt64 é o caso não representável: -(2^63) estouraria
// silenciosamente (wrap), então retorna ErrOverflow.
func (m Money) Negate() (Money, error) {
	if err := m.validate(); err != nil {
		return Money{}, err
	}
	neg, ok := negChecked(m.amountMinor)
	if !ok {
		return Money{}, fmt.Errorf("%w: negating %s", ErrOverflow, m)
	}
	return Money{amountMinor: neg, currency: m.currency}, nil
}

// Compare devolve -1, 0 ou 1. Comparar moedas diferentes é erro de contrato, não
// ordenação arbitrária — daí o (int, error).
func (m Money) Compare(other Money) (int, error) {
	if err := m.validate(); err != nil {
		return 0, err
	}
	if err := other.validate(); err != nil {
		return 0, err
	}
	if !m.currency.Equal(other.currency) {
		return 0, fmt.Errorf("%w: cannot compare %s with %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	switch {
	case m.amountMinor < other.amountMinor:
		return -1, nil
	case m.amountMinor > other.amountMinor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal compara moeda e quantidade. Moedas diferentes não são iguais (não é erro).
func (m Money) Equal(other Money) bool {
	return m.currency == other.currency && m.amountMinor == other.amountMinor
}

func (m Money) IsZero() bool { return m.amountMinor == 0 }

// IsPositive: BET/WIN/REFUND/ROLLBACK exigem valor > 0 (§7).
func (m Money) IsPositive() bool { return m.amountMinor > 0 }

func (m Money) IsNegative() bool { return m.amountMinor < 0 }

// String devolve a forma canônica decimal com exatamente 2 casas.
// Exemplos: 2500 -> "25.00"; -5 -> "-0.05"; 0 -> "0.00".
func (m Money) String() string {
	// Absoluto em uint64 para não estourar em math.MinInt64.
	abs := absUint64(m.amountMinor)
	whole := abs / 100
	frac := abs % 100
	sign := ""
	if m.amountMinor < 0 {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d", sign, whole, frac)
}

// moneyJSON é o envelope do contrato externo (§9):
//
//	{"amount":"25.00","currency":"BRL"}
//
// `amount` é string de propósito: número JSON viraria float64 no consumidor (§5.1).
type moneyJSON struct {
	Amount   string   `json:"amount"`
	Currency Currency `json:"currency"`
}

// MarshalJSON serializa no contrato externo. Money não inicializado falharia com
// {"currency":""} — melhor falhar alto.
func (m Money) MarshalJSON() ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(moneyJSON{Amount: m.String(), Currency: m.currency})
}

// UnmarshalJSON faz parsing estrito: DisallowUnknownFields recusa campos extras e
// `amount` deve ser string, então número JSON no payload falha aqui (§5.1).
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw struct {
		Amount   string   `json:"amount"`
		Currency Currency `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return fmt.Errorf("%w: cannot decode money object: %v", ErrInvalidAmount, err)
	}
	currency, err := NewCurrency(string(raw.Currency))
	if err != nil {
		return err
	}
	parsed, err := Parse(raw.Amount, currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}

// addChecked soma a+b devolvendo ok=false se houver overflow de int64.
// Se `b` é positivo, estoura quando a > MaxInt64 - b (reescrito para evitar a soma).
func addChecked(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// subChecked calcula a-b devolvendo ok=false se houver overflow.
func subChecked(a, b int64) (int64, bool) {
	// a - b == a + (-b); valida os dois sinais de `b`.
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	return a - b, true
}

// mulChecked multiplica a*b devolvendo ok=false se houver overflow.
// Usa a reconstrução (product/b != a) mais os dois casos extremos que a divisão não cobre.
func mulChecked(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == math.MinInt64 && b == -1 {
		return 0, false
	}
	if b == math.MinInt64 && a == -1 {
		return 0, false
	}
	product := a * b
	if product/b != a {
		return 0, false
	}
	return product, true
}

// negChecked nega o valor detectando o caso não representável MinInt64.
func negChecked(a int64) (int64, bool) {
	if a == math.MinInt64 {
		return 0, false
	}
	return -a, true
}

// absUint64 devolve o valor absoluto como uint64. (-(a+1))+1 é seguro mesmo em MinInt64.
func absUint64(a int64) uint64 {
	if a < 0 {
		return uint64(-(a + 1)) + 1
	}
	return uint64(a)
}
