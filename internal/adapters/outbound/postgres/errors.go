// Traduz erros do driver (pgx/PostgreSQL) para os sentinelas da aplicação via
// errors.As (o erro de constraint é *pgconn.PgError, com Code/ConstraintName). Nenhum
// caso de uso conhece o SQLSTATE do Postgres.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

// Códigos SQLSTATE do PostgreSQL que nos interessam.
//
// Documentação: https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	// sqlStateUniqueViolation: violação de índice único (23505).
	sqlStateUniqueViolation = "23505"

	// sqlStateForeignKeyViolation: FK inexistente (23503).
	sqlStateForeignKeyViolation = "23503"

	// sqlStateCheckViolation: CHECK do schema recusou o valor (23514) —
	// o backstop das invariantes financeiras (§5.3).
	sqlStateCheckViolation = "23514"

	// sqlStateSerializationFailure e sqlStateDeadlockDetected são falhas de
	// CONCORRÊNCIA: a transação pode ser repetida com sucesso.
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"

	// sqlStateObjectNotInPrerequisiteState (55000) é o que o TRIGGER do ledger
	// levanta ao tentar UPDATE/DELETE/TRUNCATE (§5.5).
	sqlStateObjectNotInPrerequisiteState = "55000"
)

// mapError converte um erro do driver no sentinela correspondente. Devolve nil para
// nil, permitindo `return mapError(err)` sem `if err != nil`.
func mapError(err error) error {
	if err == nil {
		return nil
	}

	// Linha inexistente é o caso mais comum e tem erro dedicado no pgx.
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ErrNotFound
	}

	// Cancelamento e deadline vêm do CONTEXTO, não do banco: são transitórios.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ports.ErrUnavailable, err)
	}

	// Falha de CONEXÃO (banco fora, DNS, TLS): transitória por definição.
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return fmt.Errorf("%w: %v", ports.ErrUnavailable, err)
	}

	// Erro estruturado do SERVIDOR: inspecionamos o SQLSTATE.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlStateUniqueViolation:
			// Preserva o nome da constraint: distingue "chave de idempotência repetida"
			// de "(playerId, currency) repetido" no log.
			return fmt.Errorf("%w: constraint %q", ports.ErrAlreadyExists, pgErr.ConstraintName)

		case sqlStateForeignKeyViolation:
			return fmt.Errorf("%w: constraint %q", ports.ErrNotFound, pgErr.ConstraintName)

		case sqlStateCheckViolation:
			// Um CHECK recusou o valor: a aplicação deixou passar algo inválido. É um bug
			// de domínio — o erro deve ser visível, não absorvido.
			return fmt.Errorf("%w: check %q violated: %s",
				ports.ErrConflict, pgErr.ConstraintName, pgErr.Message)

		case sqlStateSerializationFailure, sqlStateDeadlockDetected:
			// Concorrência: o chamador pode repetir. Não é "conflito de negócio".
			return fmt.Errorf("%w: %s", ports.ErrConflict, pgErr.Message)

		case sqlStateObjectNotInPrerequisiteState:
			// Nosso trigger de append-only do ledger (§5.5).
			return fmt.Errorf("%w: ledger is append-only: %s", ports.ErrConflict, pgErr.Message)
		}

		// SQLSTATE inesperado: preserva o erro original (com contexto) sem mascará-lo.
		return fmt.Errorf("postgres: %s (%s): %w", pgErr.Message, pgErr.Code, err)
	}

	return err
}
