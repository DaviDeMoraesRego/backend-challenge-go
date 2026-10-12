// Muitas colunas de `wager_transactions` são nuláveis (OPENING sem provedor, transação
// não processada sem processed_at, REJECTED sem saldo observado): lemos NULL para
// ponteiros e convertemos com helpers (derefString etc.). UUIDs usam cast explícito
// (`$1::uuid` no INSERT, `id::text` na leitura) para não depender da inferência do pgx.
package postgres

import (
	"context"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// transactionColumns é a projeção única de leitura (`::text` nos UUIDs).
const transactionColumns = `
	id::text, origin, kind, state,
	player_id::text, wallet_id::text, currency, amount_minor,
	provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
	reference_external_transaction_id, reference_transaction_id::text, correlation_id,
	failure_code, result_balance_minor, attempts, next_retry_at,
	created_at, updated_at, processed_at`

type wagerTransactionRepository struct {
	q querier
}

var _ ports.WagerTransactionRepository = (*wagerTransactionRepository)(nil)

// Insert grava a transação já em seu estado FINAL (PROCESSED/REJECTED/PENDING_REFERENCE):
// o caso de uso aplica as transições em memória e persiste uma única vez (§6.3 permite
// concluir operações sem dependências sem commit intermediário de aceite).
func (r *wagerTransactionRepository) Insert(ctx context.Context, tx *transaction.Transaction) error {
	const query = `
		INSERT INTO wager_transactions (
			id, origin, kind, state,
			player_id, wallet_id, currency, amount_minor,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
			reference_external_transaction_id, reference_transaction_id, correlation_id,
			failure_code, result_balance_minor, attempts, next_retry_at,
			created_at, updated_at, processed_at
		) VALUES (
			$1::uuid, $2, $3, $4,
			$5::uuid, $6::uuid, $7, $8,
			$9, $10, $11, $12, $13, $14,
			$15, $16::uuid, $17,
			$18, $19, $20, $21,
			$22, $23, $24
		)`

	_, err := r.q.Exec(ctx, query,
		tx.ID().String(),
		tx.Origin().String(),
		tx.Kind().String(),
		tx.State().String(),
		tx.PlayerID().String(),
		tx.WalletID().String(),
		tx.Amount().Currency().String(),
		tx.Amount().AmountMinor(),
		nullString(tx.ProviderID().String()),
		nullString(tx.ExternalTransactionID().String()),
		nullString(tx.IdempotencyKey().String()),
		nullString(tx.PayloadHash().String()),
		nullString(tx.RoundID()),
		nullString(tx.GameID()),
		nullString(tx.ReferenceExternalTransactionID().String()),
		nullUUID(tx.ReferenceTransactionID()),
		nullString(tx.CorrelationID()),
		nullString(tx.FailureCode().String()),
		nullAmount(tx.ResultBalance()),
		tx.Attempts(),
		nullTime(tx.NextRetryAt()),
		tx.CreatedAt(),
		tx.UpdatedAt(),
		nullTime(tx.ProcessedAt()),
	)
	// Violação de UNIQUE vira ports.ErrAlreadyExists: gatilho do REPLAY (§9).
	return mapError(err)
}

// Update persiste as transições e os campos que elas alteram. Não atualiza payload_hash
// nem metadados: a identidade da operação é imutável (mudar o hash quebraria o replay).
func (r *wagerTransactionRepository) Update(ctx context.Context, tx *transaction.Transaction) error {
	const query = `
		UPDATE wager_transactions
		   SET state = $2,
		       reference_transaction_id = $3::uuid,
		       failure_code = $4,
		       result_balance_minor = $5,
		       attempts = $6,
		       next_retry_at = $7,
		       updated_at = $8,
		       processed_at = $9
		 WHERE id = $1::uuid`

	tag, err := r.q.Exec(ctx, query,
		tx.ID().String(),
		tx.State().String(),
		nullUUID(tx.ReferenceTransactionID()),
		nullString(tx.FailureCode().String()),
		nullAmount(tx.ResultBalance()),
		tx.Attempts(),
		nullTime(tx.NextRetryAt()),
		tx.UpdatedAt(),
		nullTime(tx.ProcessedAt()),
	)
	if err != nil {
		return mapError(err)
	}
	return garantirLinhaAfetada(tag)
}

// FindByIDempotencyKey é a PRIMEIRA consulta do fluxo de escrita (§9).
func (r *wagerTransactionRepository) FindByIDempotencyKey(ctx context.Context, key transaction.IdempotencyKey) (*transaction.Transaction, error) {
	query := `SELECT ` + transactionColumns + ` FROM wager_transactions WHERE idempotency_key = $1`
	return r.findOne(ctx, query, key.String())
}

// FindByProviderAndExternalID resolve a chave natural externa (§9/§7).
func (r *wagerTransactionRepository) FindByProviderAndExternalID(ctx context.Context, providerID transaction.ProviderID, externalID transaction.ExternalTransactionID) (*transaction.Transaction, error) {
	query := `SELECT ` + transactionColumns + `
		FROM wager_transactions
		WHERE origin = 'EXTERNAL' AND provider_id = $1 AND external_transaction_id = $2`
	return r.findOne(ctx, query, providerID.String(), externalID.String())
}

// FindByID busca pela identidade interna.
func (r *wagerTransactionRepository) FindByID(ctx context.Context, id transaction.ID) (*transaction.Transaction, error) {
	query := `SELECT ` + transactionColumns + ` FROM wager_transactions WHERE id = $1::uuid`
	return r.findOne(ctx, query, id.String())
}

// HasSuccessfulReversal responde se a referência já recebeu uma reversão confirmada
// (§7). EXISTS para na primeira linha. A garantia contra duplo estorno é o índice único
// parcial do schema (wtx_reversal_once_per_reference_uniq); esta consulta só permite
// devolver um failureCode claro em vez de uma violação de constraint crua.
func (r *wagerTransactionRepository) HasSuccessfulReversal(ctx context.Context, providerID transaction.ProviderID, referenceExternalID transaction.ExternalTransactionID) (bool, error) {
	const query = `
		SELECT EXISTS (
			SELECT 1
			  FROM wager_transactions
			 WHERE kind IN ('REFUND', 'ROLLBACK')
			   AND state = 'PROCESSED'
			   AND provider_id = $1
			   AND reference_external_transaction_id = $2
		)`

	var exists bool
	if err := r.q.QueryRow(ctx, query, providerID.String(), referenceExternalID.String()).Scan(&exists); err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

// ListDuePendingReferences devolve as pendências vencidas, ordenadas por next_retry_at
// (mais antigas primeiro — comportamento reproduzível).
//
// Não usa SKIP LOCKED de propósito: o lock duraria só a leitura, não a aplicação da
// reversão (que ocorre na transação seguinte, sobre a carteira). A coordenação real é
// o lock da carteira com releitura do estado; SKIP LOCKED daria falsa exclusividade.
func (r *wagerTransactionRepository) ListDuePendingReferences(ctx context.Context, now time.Time, limit int) ([]*transaction.Transaction, error) {
	query := `SELECT ` + transactionColumns + `
		FROM wager_transactions
		WHERE state = 'PENDING_REFERENCE' AND next_retry_at <= $1
		ORDER BY next_retry_at, id
		LIMIT $2`

	rows, err := r.q.Query(ctx, query, now, limit)
	if err != nil {
		return nil, mapError(err)
	}
	// defer rows.Close() cobre o caminho de erro no meio do laço (o pgx fecha ao esgotar).
	defer rows.Close()

	var due []*transaction.Transaction
	for rows.Next() {
		tx, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		due = append(due, tx)
	}
	// rows.Err() captura falhas de comunicação que interromperam a leitura antes do fim.
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return due, nil
}

func (r *wagerTransactionRepository) findOne(ctx context.Context, query string, args ...any) (*transaction.Transaction, error) {
	return scanTransaction(r.q.QueryRow(ctx, query, args...))
}

// transactionRow espelha as colunas, com ponteiros para as nuláveis.
type transactionRow struct {
	id                     string
	origin                 string
	kind                   string
	state                  string
	playerID               string
	walletID               string
	currency               string
	amountMinor            int64
	providerID             *string
	externalTransactionID  *string
	idempotencyKey         *string
	payloadHash            *string
	roundID                *string
	gameID                 *string
	referenceExternalID    *string
	referenceTransactionID *string
	correlationID          *string
	failureCode            *string
	resultBalanceMinor     *int64
	attempts               int32
	nextRetryAt            *time.Time
	createdAt              time.Time
	updatedAt              time.Time
	processedAt            *time.Time
}

func scanTransaction(row scannable) (*transaction.Transaction, error) {
	var r transactionRow

	if err := row.Scan(
		&r.id, &r.origin, &r.kind, &r.state,
		&r.playerID, &r.walletID, &r.currency, &r.amountMinor,
		&r.providerID, &r.externalTransactionID, &r.idempotencyKey, &r.payloadHash, &r.roundID, &r.gameID,
		&r.referenceExternalID, &r.referenceTransactionID, &r.correlationID,
		&r.failureCode, &r.resultBalanceMinor, &r.attempts, &r.nextRetryAt,
		&r.createdAt, &r.updatedAt, &r.processedAt,
	); err != nil {
		return nil, mapError(err)
	}

	return r.toDomain()
}

// toDomain traduz a linha para o agregado, REVALIDANDO o estado (§6).
func (r transactionRow) toDomain() (*transaction.Transaction, error) {
	amount, err := money.New(r.amountMinor, money.Currency(r.currency))
	if err != nil {
		return nil, err
	}
	id, err := transaction.NewID(r.id)
	if err != nil {
		return nil, err
	}
	playerID, err := wallet.NewPlayerID(r.playerID)
	if err != nil {
		return nil, err
	}
	walletID, err := wallet.NewID(r.walletID)
	if err != nil {
		return nil, err
	}

	// Saldo observado: presente apenas quando aplicável (§6.3, "quando aplicável").
	resultBalance := transaction.NoMoney()
	if r.resultBalanceMinor != nil {
		observed, err := money.New(*r.resultBalanceMinor, money.Currency(r.currency))
		if err != nil {
			return nil, err
		}
		resultBalance = transaction.SomeMoney(observed)
	}

	// Referência interna resolvida: só existe em reversões já vinculadas.
	referenceTransactionID := transaction.NoID()
	if r.referenceTransactionID != nil {
		referenceID, err := transaction.NewID(*r.referenceTransactionID)
		if err != nil {
			return nil, err
		}
		referenceTransactionID = transaction.SomeID(referenceID)
	}

	return transaction.Rehydrate(transaction.RehydrateParams{
		ID:                             id,
		Origin:                         transaction.Origin(r.origin),
		Kind:                           transaction.Kind(r.kind),
		State:                          transaction.State(r.state),
		PlayerID:                       playerID,
		WalletID:                       walletID,
		Amount:                         amount,
		ProviderID:                     transaction.ProviderID(derefString(r.providerID)),
		ExternalTransactionID:          transaction.ExternalTransactionID(derefString(r.externalTransactionID)),
		IdempotencyKey:                 transaction.IdempotencyKey(derefString(r.idempotencyKey)),
		PayloadHash:                    transaction.PayloadHash(derefString(r.payloadHash)),
		RoundID:                        derefString(r.roundID),
		GameID:                         derefString(r.gameID),
		CorrelationID:                  derefString(r.correlationID),
		ReferenceExternalTransactionID: transaction.ExternalTransactionID(derefString(r.referenceExternalID)),
		ReferenceTransactionID:         referenceTransactionID,
		FailureCode:                    transaction.FailureCode(derefString(r.failureCode)),
		ResultBalance:                  resultBalance,
		Attempts:                       int(r.attempts),
		NextRetryAt:                    derefTime(r.nextRetryAt),
		CreatedAt:                      r.createdAt,
		UpdatedAt:                      r.updatedAt,
		ProcessedAt:                    derefTime(r.processedAt),
	})
}

// scannable é satisfeita por pgx.Row e pgx.Rows: reusa os scans em linha única e múltipla.
type scannable interface {
	Scan(dest ...any) error
}

// nullString devolve nil (SQL NULL) para string vazia.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTime devolve nil para o instante zero (o domínio usa time.Time{} como "ausente").
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// nullUUID devolve nil quando o OptionalID está ausente.
func nullUUID(id transaction.OptionalID) any {
	if !id.Valid {
		return nil
	}
	return id.Value.String()
}

// nullAmount devolve nil quando não há saldo observado.
func nullAmount(m transaction.OptionalMoney) any {
	if !m.Valid {
		return nil
	}
	return m.Value.AmountMinor()
}

// derefString lê um *string possivelmente nil.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// derefTime lê um *time.Time possivelmente nil, devolvendo o zero do Go.
func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}
