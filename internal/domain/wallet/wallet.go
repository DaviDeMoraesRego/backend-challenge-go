package wallet

import (
	"errors"
	"fmt"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
)

var (
	ErrUninitialized = errors.New("wallet: uninitialized value")
	ErrInvalidID     = errors.New("wallet: invalid id")
	ErrInvalidPlayer = errors.New("wallet: invalid player id")

	// ErrNegativeInitialBalance: a abertura não aceita saldo inicial negativo.
	ErrNegativeInitialBalance = errors.New("wallet: negative initial balance")

	// ErrNonPositiveAmount: valor <= 0. BET/WIN/REFUND/ROLLBACK exigem > 0 (§7);
	// LOSS tem valor 0 e por isso não chama Debit/Credit.
	ErrNonPositiveAmount = errors.New("wallet: amount must be strictly positive")

	// ErrInsufficientFunds: débito maior que o saldo disponível (§7).
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")

	ErrInvalidVersion   = errors.New("wallet: invalid version")
	ErrInvalidBalance   = errors.New("wallet: negative balance")
	ErrInvalidTimestamp = errors.New("wallet: invalid timestamp")
)

// ID é a identidade da carteira (formato opaco; o UUIDv7 é gerado pelo adapter).
type ID string

// PlayerID identifica o jogador dono da carteira.
type PlayerID string

// NewID valida estruturalmente a identidade (não vazia, sem espaços), não o formato.
func NewID(raw string) (ID, error) {
	if isBlank(raw) {
		return "", fmt.Errorf("%w: must not be empty", ErrInvalidID)
	}
	return ID(raw), nil
}

func NewPlayerID(raw string) (PlayerID, error) {
	if isBlank(raw) {
		return "", fmt.Errorf("%w: must not be empty", ErrInvalidPlayer)
	}
	return PlayerID(raw), nil
}

func (id ID) String() string { return string(id) }

func (p PlayerID) String() string { return string(p) }

// isBlank evita a alocação de strings.TrimSpace (regra única e reutilizável).
func isBlank(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' && s[i] != '\r' && s[i] != '\n' {
			return false
		}
	}
	return true
}

// Direction indica o sentido de uma movimentação. O texto casa exatamente com o
// CHECK da coluna `direction` em `wallet_ledger_entries` (sem tradução no adapter).
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

func (d Direction) String() string { return string(d) }

// Movement descreve exatamente o que uma movimentação fez. Alimenta o lançamento
// do ledger (§6.4) e o evento WalletBalanceChanged (§11) sem recálculo externo.
type Movement struct {
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	version       int64
}

func (m Movement) Direction() Direction { return m.direction }

func (m Movement) Amount() money.Money { return m.amount }

func (m Movement) BalanceBefore() money.Money { return m.balanceBefore }

func (m Movement) BalanceAfter() money.Money { return m.balanceAfter }

func (m Movement) WalletVersion() int64 { return m.version }

// NewOpeningMovement descreve a movimentação de abertura: saldo anterior zero,
// posterior igual ao saldo inicial e versão 1 (a abertura faz parte da criação,
// não é uma mudança subsequente de saldo — §6.2/§9). O chamador deve checar
// `initialBalance.IsPositive()` antes: saldo inicial zero não gera OPENING.
func NewOpeningMovement(initialBalance money.Money) (Movement, error) {
	if initialBalance.Currency().IsZero() {
		return Movement{}, fmt.Errorf("%w: opening balance has no currency", money.ErrUninitialized)
	}
	if !initialBalance.IsPositive() {
		// Zero ou negativo na abertura não é movimentação válida (§7/§9).
		return Movement{}, fmt.Errorf("%w: opening balance %s", ErrNonPositiveAmount, initialBalance)
	}
	zero, err := money.Zero(initialBalance.Currency())
	if err != nil {
		return Movement{}, err
	}
	return Movement{
		direction:     Credit,
		amount:        initialBalance,
		balanceBefore: zero,
		balanceAfter:  initialBalance,
		version:       1,
	}, nil
}

// Wallet é a raiz do agregado financeiro (§6.2). Campos não-exportados: toda
// regra de saldo passa por Debit/Credit e o adapter Postgres reidrata via métodos.
type Wallet struct {
	id        ID
	playerID  PlayerID
	currency  money.Currency
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// New cria uma carteira nova com saldo inicial. A moeda da carteira É a do saldo
// inicial (não há parâmetro separado, então não podem divergir). Saldo inicial zero
// cria a carteira sem OPENING, ledger ou eventos (§9) — decisão da camada de aplicação.
func New(id ID, playerID PlayerID, initialBalance money.Money, now time.Time) (*Wallet, error) {
	if isBlank(string(id)) {
		return nil, fmt.Errorf("%w: must not be empty", ErrInvalidID)
	}
	if isBlank(string(playerID)) {
		return nil, fmt.Errorf("%w: must not be empty", ErrInvalidPlayer)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: creation time is required", ErrInvalidTimestamp)
	}
	// O saldo inicial vem de fora e pode estar não inicializado (moeda vazia).
	if initialBalance.Currency().IsZero() {
		return nil, fmt.Errorf("%w: initial balance has no currency", money.ErrUninitialized)
	}
	if initialBalance.IsNegative() {
		return nil, fmt.Errorf("%w: %s", ErrNegativeInitialBalance, initialBalance)
	}

	ts := now.UTC()
	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  initialBalance.Currency(),
		balance:   initialBalance,
		version:   1, // §6.2: versão inicial é 1, inclusive após o crédito de abertura
		createdAt: ts,
		updatedAt: ts,
	}, nil
}

// Rehydrate reconstrói a carteira a partir de dados persistidos. NÃO reaplica
// movimentações, transições nem eventos (§6): apenas valida o estado lido. Reidratar
// via Credit(balance) geraria lançamentos e eventos fictícios a cada leitura.
func Rehydrate(
	id ID,
	playerID PlayerID,
	balance money.Money,
	version int64,
	createdAt time.Time,
	updatedAt time.Time,
) (*Wallet, error) {
	if isBlank(string(id)) {
		return nil, fmt.Errorf("%w: must not be empty", ErrInvalidID)
	}
	if isBlank(string(playerID)) {
		return nil, fmt.Errorf("%w: must not be empty", ErrInvalidPlayer)
	}
	if balance.Currency().IsZero() {
		return nil, fmt.Errorf("%w: hydrated balance has no currency", money.ErrUninitialized)
	}
	// Saldo negativo persistido é dado corrompido.
	if balance.IsNegative() {
		return nil, fmt.Errorf("%w: %s", ErrInvalidBalance, balance)
	}
	if version < 1 {
		return nil, fmt.Errorf("%w: version must be >= 1, got %d", ErrInvalidVersion, version)
	}
	if createdAt.IsZero() || updatedAt.IsZero() {
		return nil, fmt.Errorf("%w: timestamps are required", ErrInvalidTimestamp)
	}

	return &Wallet{
		id:        id,
		playerID:  playerID,
		currency:  balance.Currency(),
		balance:   balance,
		version:   version,
		createdAt: createdAt.UTC(),
		updatedAt: updatedAt.UTC(),
	}, nil
}

func (w *Wallet) ID() ID { return w.id }

func (w *Wallet) PlayerID() PlayerID { return w.playerID }

func (w *Wallet) Currency() money.Currency { return w.currency }

// Balance devolve Money por valor — imutável, não há como alterar o saldo interno.
func (w *Wallet) Balance() money.Money { return w.balance }

// Version: 1 na criação; incrementa apenas quando o saldo muda (§6.2).
func (w *Wallet) Version() int64 { return w.version }

func (w *Wallet) CreatedAt() time.Time { return w.createdAt }

func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

func (w *Wallet) HasBalance() bool { return w.balance.IsPositive() }

// Debit reduz o saldo respeitando as invariantes do §6.2 e devolve a descrição
// exata da movimentação (ledger e eventos). Receiver de ponteiro: muta o estado.
func (w *Wallet) Debit(amount money.Money, now time.Time) (Movement, error) {
	var zero Movement

	if err := w.validateForMovement(amount, now); err != nil {
		return zero, err
	}

	// Compare já valida moeda compatível.
	comparison, err := w.balance.Compare(amount)
	if err != nil {
		return zero, err
	}
	if comparison < 0 {
		// §6.2/§7: débito não pode levar o saldo abaixo de zero.
		return zero, fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, w.balance, amount)
	}

	// Sub já validou moeda; o erro restante só pode ser overflow.
	newBalance, err := w.balance.Sub(amount)
	if err != nil {
		return zero, err
	}

	// Saldo anterior guardado antes de sobrescrever (ledger e evento exigem).
	before := w.balance
	ts := now.UTC()

	w.balance = newBalance
	w.version++ // §6.2: incrementa somente porque houve mudança de saldo
	w.updatedAt = ts

	return Movement{
		direction:     Debit,
		amount:        amount,
		balanceBefore: before,
		balanceAfter:  newBalance,
		version:       w.version,
	}, nil
}

// Credit aumenta o saldo. Mesma estrutura de Debit, mas sem validação de saldo
// suficiente.
func (w *Wallet) Credit(amount money.Money, now time.Time) (Movement, error) {
	var zero Movement

	if err := w.validateForMovement(amount, now); err != nil {
		return zero, err
	}

	// Add valida moeda compatível e detecta overflow.
	newBalance, err := w.balance.Add(amount)
	if err != nil {
		return zero, err
	}

	before := w.balance
	ts := now.UTC()

	w.balance = newBalance
	w.version++
	w.updatedAt = ts

	return Movement{
		direction:     Credit,
		amount:        amount,
		balanceBefore: before,
		balanceAfter:  newBalance,
		version:       w.version,
	}, nil
}

// validateForMovement centraliza as pré-condições comuns a débito e crédito.
// As regras específicas (saldo suficiente no débito) ficam no método respectivo.
func (w *Wallet) validateForMovement(amount money.Money, now time.Time) error {
	if w == nil {
		// Receiver de ponteiro pode ser chamado em nil; sem isso, w.currency daria panic.
		return ErrUninitialized
	}
	if w.id == "" {
		return ErrUninitialized
	}
	if amount.Currency().IsZero() {
		return fmt.Errorf("%w: movement has no currency", money.ErrUninitialized)
	}
	if !amount.IsPositive() {
		// §7: BET/WIN/REFUND/ROLLBACK exigem valor > 0 (LOSS não movimenta).
		return fmt.Errorf("%w: got %s", ErrNonPositiveAmount, amount)
	}
	if !amount.Currency().Equal(w.currency) {
		// §6.2: moeda da movimentação deve coincidir com a da carteira.
		return fmt.Errorf("%w: wallet is %s, movement is %s", money.ErrCurrencyMismatch, w.currency, amount.Currency())
	}
	if now.IsZero() {
		return fmt.Errorf("%w: movement time is required", ErrInvalidTimestamp)
	}
	return nil
}
