// Concorrência pessimista por carteira: `SELECT ... FOR UPDATE` bloqueia a linha até
// o fim da transação — dois débitos na mesma carteira são serializados em qualquer
// processo, carteiras diferentes não se bloqueiam (§5.6) e o lock é liberado no
// commit/rollback (inclusive se o processo morrer). As constraints do schema
// (CHECK balance_minor >= 0, unicidade do ledger) são o backstop (§5.3).
package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// walletColumns é a projeção usada em todas as leituras: manter a lista num só lugar
// evita o bug de um SELECT com colunas em ordem diferente do Scan (que só falha em runtime).
const walletColumns = `id, player_id, currency, balance_minor, version, created_at, updated_at`

type walletRepository struct {
	q querier
}

var _ ports.WalletRepository = (*walletRepository)(nil)

// Insert cria a carteira. Não fazemos "verificar e depois inserir": o UNIQUE
// (player_id, currency) decide — mais rápido e sem corrida entre consulta e inserção.
func (r *walletRepository) Insert(ctx context.Context, w *wallet.Wallet) error {
	const query = `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// placeholders numerados + binding: a proteção contra SQL injection vem da
	// separação entre comando e dados.
	_, err := r.q.Exec(ctx, query,
		w.ID().String(),
		w.PlayerID().String(),
		w.Currency().String(),
		w.Balance().AmountMinor(), // dinheiro SEMPRE em unidades mínimas (§5.1)
		w.Version(),
		w.CreatedAt(),
		w.UpdatedAt(),
	)
	return mapError(err)
}

// FindByPlayerAndCurrency busca pela chave natural (playerId, currency).
func (r *walletRepository) FindByPlayerAndCurrency(ctx context.Context, playerID wallet.PlayerID, currency money.Currency) (*wallet.Wallet, error) {
	query := `SELECT ` + walletColumns + ` FROM wallets WHERE player_id = $1 AND currency = $2`
	return r.findOne(ctx, query, playerID.String(), currency.String())
}

// FindByID busca por identidade, SEM lock (leitura).
func (r *walletRepository) FindByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error) {
	query := `SELECT ` + walletColumns + ` FROM wallets WHERE id = $1`
	return r.findOne(ctx, query, id.String())
}

// LockByID é o ponto de serialização por carteira (§8): toda operação que altera o
// saldo passa por aqui ANTES de ler o saldo usado na decisão.
func (r *walletRepository) LockByID(ctx context.Context, id wallet.ID) (*wallet.Wallet, error) {
	query := `SELECT ` + walletColumns + ` FROM wallets WHERE id = $1 FOR UPDATE`
	return r.findOne(ctx, query, id.String())
}

// Update persiste saldo, versão e `updatedAt`.
func (r *walletRepository) Update(ctx context.Context, w *wallet.Wallet) error {
	// Segunda linha de defesa contra lost update: detecta reutilizar um agregado lido
	// SEM passar pelo lock — a versão não avançou, o UPDATE não encontra a linha e
	// vira ErrNotFound em vez de sobrescrever um saldo que já mudou.
	const query = `
		UPDATE wallets
		   SET balance_minor = $2, version = $3, updated_at = $4
		 WHERE id = $1 AND version < $3`

	tag, err := r.q.Exec(ctx, query,
		w.ID().String(),
		w.Balance().AmountMinor(),
		w.Version(),
		w.UpdatedAt(),
	)
	if err != nil {
		return mapError(err)
	}
	return garantirLinhaAfetada(tag)
}

func (r *walletRepository) findOne(ctx context.Context, query string, args ...any) (*wallet.Wallet, error) {
	// QueryRow fecha as linhas internamente (dispensa defer rows.Close()).
	row := r.q.QueryRow(ctx, query, args...)
	return scanWallet(row)
}

// scanWallet converte a linha no agregado e REVALIDA as invariantes via Rehydrate: um
// estado impossível (saldo negativo, versão zero) falha na leitura, perto da causa.
func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		idRaw        string
		playerIDRaw  string
		currencyRaw  string
		balanceMinor int64
		version      int64
		createdAt    time.Time
		updatedAt    time.Time
	)

	if err := row.Scan(&idRaw, &playerIDRaw, &currencyRaw, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		return nil, mapError(err)
	}

	balance, err := money.New(balanceMinor, money.Currency(currencyRaw))
	if err != nil {
		return nil, err
	}
	walletID, err := wallet.NewID(idRaw)
	if err != nil {
		return nil, err
	}
	playerID, err := wallet.NewPlayerID(playerIDRaw)
	if err != nil {
		return nil, err
	}

	// Rehydrate (não New): não reaplica movimentações nem emite eventos (§6).
	return wallet.Rehydrate(walletID, playerID, balance, version, createdAt, updatedAt)
}
