package ledger

import (
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

var (
	ErrUninitialized        = fmt.Errorf("ledger: uninitialized value")
	ErrMissingWalletID      = fmt.Errorf("ledger: missing wallet id")
	ErrMissingTransactionID = fmt.Errorf("ledger: missing transaction id")
	ErrInvalidDirection     = fmt.Errorf("ledger: invalid direction")

	// ErrNonPositiveAmount: o ledger só registra movimentação efetiva; LOSS não gera entrada.
	ErrNonPositiveAmount = fmt.Errorf("ledger: amount must be strictly positive")

	// ErrBalanceChainViolation: balanceAfter não corresponde a balanceBefore ± amount.
	ErrBalanceChainViolation = fmt.Errorf("ledger: balance chain violation")

	ErrNegativeBalance  = fmt.Errorf("ledger: balance after must not be negative")
	ErrCurrencyMismatch = fmt.Errorf("ledger: currency mismatch")
	ErrInvalidTimestamp = fmt.Errorf("ledger: invalid timestamp")
	ErrInvalidID        = fmt.Errorf("ledger: invalid id")
)

// Entry é o lançamento imutável do ledger (§6.4).
type Entry struct {
	id            int64 // bigserial — define a ordem estável do extrato
	walletID      wallet.ID
	transactionID transaction.ID
	direction     wallet.Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// NewEntryParams reúne os campos para construir um lançamento explicitamente.
type NewEntryParams struct {
	WalletID      wallet.ID
	TransactionID transaction.ID
	Direction     wallet.Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	OccurredAt    time.Time
}

// NewEntry constrói e valida um lançamento. Prefira NewEntryFromMovement quando
// houver um wallet.Movement disponível.
func NewEntry(p NewEntryParams) (*Entry, error) {
	if err := validateCore(p); err != nil {
		return nil, err
	}

	return &Entry{
		// id fica zero: atribuído pelo banco (bigserial) na inserção.
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		createdAt:     p.OccurredAt.UTC(),
	}, nil
}

// NewEntryFromMovement é o construtor preferencial: deriva tudo do movimento que
// a carteira já produziu, eliminando divergência entre carteira e ledger.
func NewEntryFromMovement(
	walletID wallet.ID,
	transactionID transaction.ID,
	movement wallet.Movement,
	occurredAt time.Time,
) (*Entry, error) {
	return NewEntry(NewEntryParams{
		WalletID:      walletID,
		TransactionID: transactionID,
		Direction:     movement.Direction(),
		Amount:        movement.Amount(),
		BalanceBefore: movement.BalanceBefore(),
		BalanceAfter:  movement.BalanceAfter(),
		OccurredAt:    occurredAt,
	})
}

// RehydrateParams reúne o estado persistido de um lançamento.
type RehydrateParams struct {
	ID            int64
	WalletID      wallet.ID
	TransactionID transaction.ID
	Direction     wallet.Direction
	Amount        money.Money
	BalanceBefore money.Money
	BalanceAfter  money.Money
	CreatedAt     time.Time
}

// Rehydrate reconstrói um lançamento lido do banco, revalidando a cadeia de saldo.
func Rehydrate(p RehydrateParams) (*Entry, error) {
	if p.ID <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidID, p.ID)
	}
	if err := validateCore(NewEntryParams{
		WalletID:      p.WalletID,
		TransactionID: p.TransactionID,
		Direction:     p.Direction,
		Amount:        p.Amount,
		BalanceBefore: p.BalanceBefore,
		BalanceAfter:  p.BalanceAfter,
		OccurredAt:    p.CreatedAt,
	}); err != nil {
		return nil, err
	}

	return &Entry{
		id:            p.ID,
		walletID:      p.WalletID,
		transactionID: p.TransactionID,
		direction:     p.Direction,
		amount:        p.Amount,
		balanceBefore: p.BalanceBefore,
		balanceAfter:  p.BalanceAfter,
		createdAt:     p.CreatedAt.UTC(),
	}, nil
}

// validateCore concentra as invariantes do lançamento (ponto único de verdade).
func validateCore(p NewEntryParams) error {
	if isBlank(string(p.WalletID)) {
		return ErrMissingWalletID
	}
	if isBlank(string(p.TransactionID)) {
		return ErrMissingTransactionID
	}
	if p.OccurredAt.IsZero() {
		return ErrInvalidTimestamp
	}
	if p.Direction != wallet.Debit && p.Direction != wallet.Credit {
		return fmt.Errorf("%w: %q", ErrInvalidDirection, p.Direction)
	}

	// Valor e os dois saldos precisam ser da MESMA moeda.
	if p.Amount.Currency().IsZero() || p.BalanceBefore.Currency().IsZero() || p.BalanceAfter.Currency().IsZero() {
		return fmt.Errorf("%w: amount or balances have no currency", money.ErrUninitialized)
	}
	if !p.Amount.Currency().Equal(p.BalanceBefore.Currency()) ||
		!p.Amount.Currency().Equal(p.BalanceAfter.Currency()) {
		return fmt.Errorf("%w: amount=%s before=%s after=%s",
			ErrCurrencyMismatch, p.Amount.Currency(), p.BalanceBefore.Currency(), p.BalanceAfter.Currency())
	}

	// Valor sempre positivo: o ledger registra movimentação efetiva (§6.4).
	if !p.Amount.IsPositive() {
		return fmt.Errorf("%w: got %s", ErrNonPositiveAmount, p.Amount)
	}

	// §6.4: cadeia de saldo é a invariante central, calculada com a aritmética do Money.
	var expectedAfter money.Money
	var err error
	switch p.Direction {
	case wallet.Credit:
		expectedAfter, err = p.BalanceBefore.Add(p.Amount)
	case wallet.Debit:
		expectedAfter, err = p.BalanceBefore.Sub(p.Amount)
	}
	if err != nil {
		return fmt.Errorf("ledger: cannot compute expected balance: %w", err)
	}
	if !expectedAfter.Equal(p.BalanceAfter) {
		return fmt.Errorf("%w: direction=%s before=%s amount=%s after=%s expected=%s",
			ErrBalanceChainViolation, p.Direction, p.BalanceBefore, p.Amount, p.BalanceAfter, expectedAfter)
	}

	// §6.2/§7: saldo nunca fica negativo.
	if p.BalanceAfter.IsNegative() {
		return fmt.Errorf("%w: %s", ErrNegativeBalance, p.BalanceAfter)
	}

	return nil
}

func (e *Entry) ID() int64 { return e.id }

func (e *Entry) WalletID() wallet.ID { return e.walletID }

func (e *Entry) TransactionID() transaction.ID { return e.transactionID }

func (e *Entry) Direction() wallet.Direction { return e.direction }

func (e *Entry) Amount() money.Money { return e.amount }

func (e *Entry) BalanceBefore() money.Money { return e.balanceBefore }

func (e *Entry) BalanceAfter() money.Money { return e.balanceAfter }

func (e *Entry) CreatedAt() time.Time { return e.createdAt }

func (e *Entry) IsDebit() bool { return e.direction == wallet.Debit }

func (e *Entry) IsCredit() bool { return e.direction == wallet.Credit }

// SignedAmount devolve o valor com sinal contábil (crédito positivo, débito
// negativo). É o insumo da reconciliação (§9): o saldo reconstruído é a soma de
// todos os SignedAmount, incluindo a abertura.
func (e *Entry) SignedAmount() (money.Money, error) {
	if e.direction == wallet.Credit {
		return e.amount, nil
	}
	return e.amount.Negate()
}

// isBlank: helper local (evita pacote compartilhado entre agregados).
func isBlank(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return false
		}
	}
	return true
}
