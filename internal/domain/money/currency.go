package money

import (
	"errors"
	"fmt"
)

var ErrInvalidCurrency = errors.New("money: invalid currency")

// Currency é um código ISO 4217 de 3 letras maiúsculas (CHAR(3) com CHECK no schema).
type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

// NewCurrency valida e constrói uma moeda: exatamente 3 bytes A-Z.
// Sem normalização de caixa — o hash de idempotência (§9) exige determinismo.
func NewCurrency(code string) (Currency, error) {
	if len(code) != 3 {
		return "", fmt.Errorf("%w: %q must be exactly 3 letters", ErrInvalidCurrency, code)
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 'A' || code[i] > 'Z' {
			return "", fmt.Errorf("%w: %q must contain only uppercase A-Z", ErrInvalidCurrency, code)
		}
	}
	return Currency(code), nil
}

func (c Currency) String() string { return string(c) }

// IsZero informa se a moeda não foi inicializada (§6).
func (c Currency) IsZero() bool { return c == "" }

func (c Currency) Equal(other Currency) bool { return c == other }
