// A interface `querier` (Query/QueryRow/Exec) é satisfeita tanto por *pgxpool.Pool
// quanto por pgx.Tx, mantendo os repositórios iguais dentro e fora da transação. O
// rollback de emergência usa contexto próprio (WithoutCancel): se o ctx estiver
// cancelado, Rollback(ctx) falharia e a conexão ficaria presa. Em panic, o rollback
// roda antes de o panic ser repropagado.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

// errConnectivity é usado apenas para marcar erros de inicialização do pool.
var errConnectivity = errors.New("postgres: connectivity")

// querier é o mínimo que os repositórios precisam do driver (declarada onde é
// consumida; pgx.Tx a satisfaz implicitamente).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// rollbackTimeout limita quanto tempo esperamos pelo rollback de emergência.
const rollbackTimeout = 5 * time.Second

// UnitOfWork implementa `ports.UnitOfWork` sobre o pool do pgx.
type UnitOfWork struct {
	pool *pgxpool.Pool
}

var _ ports.UnitOfWork = (*UnitOfWork)(nil)

func NewUnitOfWork(pool *pgxpool.Pool) *UnitOfWork {
	return &UnitOfWork{pool: pool}
}

// Execute roda `fn` numa transação. nil -> COMMIT; erro -> ROLLBACK devolvendo o erro
// original (não o do rollback, que não deve mascarar a causa raiz); panic -> ROLLBACK
// e o panic é repropagado.
func (u *UnitOfWork) Execute(ctx context.Context, fn func(context.Context, ports.TxScope) error) error {
	// Begin associa a transação ao contexto: cancelado o ctx, a conexão não volta
	// suja para o pool.
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return mapError(err)
	}

	// finished evita o rollback duplicado no caminho de erro (um único ponto de decisão).
	finished := false

	// defer logo após o Begin: todo caminho de saída passa por aqui, inclusive panic.
	defer func() {
		if finished {
			return
		}
		// Rollback de emergência com contexto próprio: se o ctx original estiver
		// cancelado, Rollback(ctx) falharia e a conexão ficaria presa. WithoutCancel
		// preserva os valores (tracing) e descarta o cancelamento.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	// recover roda junto do rollback (LIFO): o rollback executa antes do panic ser
	// repropagado.
	defer func() {
		if p := recover(); p != nil {
			finished = true
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
			// Reproaga: um panic é um BUG, e engoli-lo esconderia o problema.
			panic(p)
		}
	}()

	if err := fn(ctx, &txScope{tx: tx}); err != nil {
		// O erro do bloco é a causa raiz: devolvemos ele, não o do rollback.
		// O rollback acontece no defer.
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return mapError(err)
	}
	finished = true
	return nil
}

// txScope liga os repositórios à transação corrente.
type txScope struct {
	tx pgx.Tx
}

var _ ports.TxScope = (*txScope)(nil)

func (s *txScope) Wallets() ports.WalletRepository {
	return &walletRepository{q: s.tx}
}

func (s *txScope) Transactions() ports.WagerTransactionRepository {
	return &wagerTransactionRepository{q: s.tx}
}

func (s *txScope) Ledger() ports.LedgerRepository {
	return &ledgerRepository{q: s.tx}
}

func (s *txScope) Inbox() ports.InboxRepository {
	return &inboxRepository{q: s.tx}
}

func (s *txScope) Outbox() ports.OutboxRepository {
	return &outboxRepository{q: s.tx}
}

// ignorarLinhaAusente trata "sem linha" como resultado válido (ao contrário de
// mapError, que transforma ErrNoRows em ErrNotFound).
func ignorarLinhaAusente(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return mapError(err)
}

// garantirLinhaAfetada converte "nenhuma linha afetada" (RowsAffected() == 0) em
// ErrNotFound: para a aplicação, significa que o registro não existe.
func garantirLinhaAfetada(tag pgconn.CommandTag) error {
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: no rows affected", ports.ErrNotFound)
	}
	return nil
}
