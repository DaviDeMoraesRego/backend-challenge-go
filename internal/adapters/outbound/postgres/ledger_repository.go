// Ledger append-only (§5.5): não existe Update/Delete por design — correções exigem
// NOVOS lançamentos, e o banco recusa mutação por TRIGGER. A soma da reconciliação
// resolve a moeda com um JOIN na carteira.
package postgres

import (
	"context"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/ledger"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// O ledger guarda só BIGINT em unidades mínimas; a moeda vem da carteira via JOIN.
const ledgerColumns = `
	l.id, l.wallet_id::text, l.transaction_id::text, l.direction,
	l.amount_minor, l.balance_before_minor, l.balance_after_minor, l.created_at,
	w.currency`

type ledgerRepository struct {
	q querier
}

var _ ports.LedgerRepository = (*ledgerRepository)(nil)

// Append insere um lançamento. O id é atribuído pelo BIGSERIAL e não é devolvido (o
// agregado em memória segue com id zero; quem lê depois recebe ids reais do banco).
func (r *ledgerRepository) Append(ctx context.Context, entry *ledger.Entry) error {
	const query = `
		INSERT INTO wallet_ledger_entries (
			wallet_id, transaction_id, direction,
			amount_minor, balance_before_minor, balance_after_minor, created_at
		) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)`

	_, err := r.q.Exec(ctx, query,
		entry.WalletID().String(),
		entry.TransactionID().String(),
		entry.Direction().String(),
		entry.Amount().AmountMinor(),
		entry.BalanceBefore().AmountMinor(),
		entry.BalanceAfter().AmountMinor(),
		entry.CreatedAt(),
	)
	if err == nil {
		return nil
	}
	// UNIQUE (wallet_id, transaction_id) violado: o mesmo lançamento entrou duas vezes
	// — backstop contra movimentação duplicada (§5.7).
	return mapError(err)
}

// ListByWallet devolve uma página por `id` crescente. `id > afterID` é o coração da
// paginação por cursor (§9): range scan direto no índice (wallet_id, id).
func (r *ledgerRepository) ListByWallet(ctx context.Context, walletID wallet.ID, afterID int64, limit int) ([]*ledger.Entry, error) {
	// JOIN com wallets para trazer a moeda real: o ledger não a armazena.
	query := `SELECT ` + ledgerColumns + `
		FROM wallet_ledger_entries l
		JOIN wallets w ON w.id = l.wallet_id
		WHERE l.wallet_id = $1::uuid AND l.id > $2
		ORDER BY l.id
		LIMIT $3`

	rows, err := r.q.Query(ctx, query, walletID.String(), afterID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var entries []*ledger.Entry
	for rows.Next() {
		entry, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return entries, nil
}

// SumSignedAmounts reconstrói o saldo (§9) como SUM(CREDIT) - SUM(DEBIT). O JOIN com
// `wallets` traz a moeda da própria carteira (o saldo reconstruído precisa dela para
// virar Money, sem parâmetro extra sujeito a erro). COALESCE cobre a carteira sem
// lançamentos (soma de nada é zero, não NULL).
func (r *ledgerRepository) SumSignedAmounts(ctx context.Context, walletID wallet.ID) (money.Money, error) {
	const query = `
		SELECT
			COALESCE(SUM(CASE WHEN l.direction = 'CREDIT' THEN l.amount_minor ELSE -l.amount_minor END), 0),
			w.currency
		  FROM wallets w
		  LEFT JOIN wallet_ledger_entries l ON l.wallet_id = w.id
		 WHERE w.id = $1::uuid
		 GROUP BY w.currency`

	var (
		totalMinor int64
		currency   string
	)
	if err := r.q.QueryRow(ctx, query, walletID.String()).Scan(&totalMinor, &currency); err != nil {
		return money.Money{}, mapError(err)
	}

	return money.New(totalMinor, money.Currency(currency))
}

// CountByWallet devolve `checkedEntries` da reconciliação (§9).
func (r *ledgerRepository) CountByWallet(ctx context.Context, walletID wallet.ID) (int, error) {
	const query = `SELECT COUNT(*) FROM wallet_ledger_entries WHERE wallet_id = $1::uuid`

	var count int
	if err := r.q.QueryRow(ctx, query, walletID.String()).Scan(&count); err != nil {
		return 0, mapError(err)
	}
	return count, nil
}

// scanLedgerEntry reconstrói o lançamento, revalidando a cadeia de saldo.
func scanLedgerEntry(row scannable) (*ledger.Entry, error) {
	var (
		id                 int64
		walletIDRaw        string
		transactionIDRaw   string
		directionRaw       string
		amountMinor        int64
		balanceBeforeMinor int64
		balanceAfterMinor  int64
		createdAt          time.Time
		currencyRaw        string
	)

	if err := row.Scan(&id, &walletIDRaw, &transactionIDRaw, &directionRaw,
		&amountMinor, &balanceBeforeMinor, &balanceAfterMinor, &createdAt, &currencyRaw); err != nil {
		return nil, mapError(err)
	}

	currency := money.Currency(currencyRaw)
	amount, err := money.New(amountMinor, currency)
	if err != nil {
		return nil, err
	}
	balanceBefore, err := money.New(balanceBeforeMinor, currency)
	if err != nil {
		return nil, err
	}
	balanceAfter, err := money.New(balanceAfterMinor, currency)
	if err != nil {
		return nil, err
	}

	walletID, err := wallet.NewID(walletIDRaw)
	if err != nil {
		return nil, err
	}
	transactionID, err := transaction.NewID(transactionIDRaw)
	if err != nil {
		return nil, err
	}

	// Rehydrate revalida a invariante do §6.4: balanceAfter == balanceBefore ± amount.
	return ledger.Rehydrate(ledger.RehydrateParams{
		ID:            id,
		WalletID:      walletID,
		TransactionID: transactionID,
		Direction:     wallet.Direction(directionRaw),
		Amount:        amount,
		BalanceBefore: balanceBefore,
		BalanceAfter:  balanceAfter,
		CreatedAt:     createdAt,
	})
}
