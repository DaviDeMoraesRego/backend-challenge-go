// Mapa de status (contrato do §9): 201 carteira criada; 200 operação concluída
// (PROCESSED/REJECTED); 202 aguardando referência (PENDING_REFERENCE); 400 requisição
// malformada; 401 credencial inválida; 403 sem permissão (§2); 404 inexistente;
// 409 conflito de idempotência/unicidade (§9); 422 conteúdo inválido; 500 inesperado;
// 503 dependência indisponível (RETRY). Este adapter não valida regra de negócio — só
// decodifica, delega à porta e serializa; validar aqui duplicaria o caso de uso.
package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/transaction"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/metrics"
)

// Handlers reúne as portas inbound que a API expõe.
type Handlers struct {
	openWallet     ports.OpenWallet
	placeOperation ports.PlaceWagerOperation
	getWallet      ports.GetWallet
	getLedger      ports.GetWalletLedger
	getTransaction ports.GetWagerTransaction
	getByExternal  ports.GetWagerTransactionByExternalID
	reconcile      ports.ReconcileWallet
	logger         *slog.Logger
	// collector registra métricas de negócio (§12): replays, conflitos e divergências
	// de reconciliação — não deriváveis do middleware, só do ponto da decisão.
	collector *metrics.Metrics
}

func NewHandlers(
	openWallet ports.OpenWallet,
	placeOperation ports.PlaceWagerOperation,
	getWallet ports.GetWallet,
	getLedger ports.GetWalletLedger,
	getTransaction ports.GetWagerTransaction,
	getByExternal ports.GetWagerTransactionByExternalID,
	reconcile ports.ReconcileWallet,
	logger *slog.Logger,
	collector *metrics.Metrics,
) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	if collector == nil {
		collector = metrics.New()
	}
	return &Handlers{
		openWallet:     openWallet,
		placeOperation: placeOperation,
		getWallet:      getWallet,
		getLedger:      getLedger,
		getTransaction: getTransaction,
		getByExternal:  getByExternal,
		reconcile:      reconcile,
		logger:         logger,
		collector:      collector,
	}
}

type moneyPayload struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type openWalletRequest struct {
	PlayerID       string       `json:"playerId"`
	InitialBalance moneyPayload `json:"initialBalance"`
}

type walletResponse struct {
	WalletID string      `json:"id"`
	PlayerID string      `json:"playerId"`
	Balance  money.Money `json:"balance"`
	Version  int64       `json:"version"`
}

type placeOperationRequest struct {
	ProviderID                     string       `json:"providerId"`
	ExternalTransactionID          string       `json:"externalTransactionId"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	RoundID                        string       `json:"roundId"`
	GameID                         string       `json:"gameId"`
	Kind                           string       `json:"kind"`
	Money                          moneyPayload `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId"`
}

type operationResponse struct {
	TransactionID string `json:"transactionId"`
	Status        string `json:"status"`
	// `failureCode` só aparece em rejeição/falha.
	FailureCode string `json:"failureCode,omitempty"`
	// `balance` é OMITIDO quando não houve resultado financeiro a observar
	// (rejeição ou pendência). Ver `BalanceKnown` em ports.
	Balance          *money.Money `json:"balance,omitempty"`
	IdempotentReplay bool         `json:"idempotentReplay"`
}

type ledgerEntryResponse struct {
	ID            int64       `json:"id"`
	TransactionID string      `json:"transactionId"`
	Direction     string      `json:"direction"`
	Amount        money.Money `json:"amount"`
	BalanceBefore money.Money `json:"balanceBefore"`
	BalanceAfter  money.Money `json:"balanceAfter"`
	CreatedAt     time.Time   `json:"createdAt"`
}

type ledgerResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

type reconciliationResponse struct {
	WalletID          string      `json:"walletId"`
	StoredBalance     money.Money `json:"storedBalance"`
	CalculatedBalance money.Money `json:"calculatedBalance"`
	Difference        money.Money `json:"difference"`
	Consistent        bool        `json:"consistent"`
	CheckedEntries    int         `json:"checkedEntries"`
}

type transactionResponse struct {
	TransactionID                  string       `json:"transactionId"`
	ProviderID                     string       `json:"providerId,omitempty"`
	ExternalTransactionID          string       `json:"externalTransactionId,omitempty"`
	PlayerID                       string       `json:"playerId"`
	WalletID                       string       `json:"walletId"`
	RoundID                        string       `json:"roundId,omitempty"`
	GameID                         string       `json:"gameId,omitempty"`
	Kind                           string       `json:"kind"`
	Status                         string       `json:"status"`
	Money                          money.Money  `json:"money"`
	ReferenceExternalTransactionID string       `json:"referenceExternalTransactionId,omitempty"`
	FailureCode                    string       `json:"failureCode,omitempty"`
	Correctable                    bool         `json:"correctable,omitempty"`
	Balance                        *money.Money `json:"balance,omitempty"`
	Attempts                       int          `json:"attempts"`
	NextRetryAt                    *time.Time   `json:"nextRetryAt,omitempty"`
	CreatedAt                      time.Time    `json:"createdAt"`
	UpdatedAt                      time.Time    `json:"updatedAt"`
	ProcessedAt                    *time.Time   `json:"processedAt,omitempty"`
}

// -----------------------------------------------------------------------------
// POST /wallets
// -----------------------------------------------------------------------------

// handleOpenWallet trata POST /wallets.
//
// Os métodos levam o prefixo `handle`: em Go, campo e método compartilham o espaço de
// nomes do tipo, então `handleX` evita o erro "field and method with the same name".
func (h *Handlers) handleOpenWallet(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	var request openWalletRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	result, err := h.openWallet.Execute(r.Context(), ports.OpenWalletCommand{
		Principal:       principal,
		PlayerID:        request.PlayerID,
		InitialAmount:   request.InitialBalance.Amount,
		InitialCurrency: request.InitialBalance.Currency,
		CorrelationID:   correlationIDFromContext(r.Context()),
	})
	if err != nil {
		writeOperationError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusCreated, walletResponse{
		WalletID: result.WalletID.String(),
		PlayerID: result.PlayerID.String(),
		Balance:  result.Balance,
		Version:  result.Version,
	})
}

// -----------------------------------------------------------------------------
// POST /wagering/transactions
// -----------------------------------------------------------------------------

// handlePlaceOperation trata POST /wagering/transactions.
func (h *Handlers) handlePlaceOperation(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	// §9: o header é OBRIGATÓRIO. O servidor não substitui uma chave recebida por
	// outra calculada — por isso a ausência é ERRO, não um default silencioso.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_idempotency_key",
			"the Idempotency-Key header is required")
		return
	}

	var request placeOperationRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}

	result, err := h.placeOperation.Execute(r.Context(), ports.PlaceOperationCommand{
		Principal:                      principal,
		ProviderID:                     request.ProviderID,
		ExternalTransactionID:          request.ExternalTransactionID,
		IdempotencyKey:                 idempotencyKey,
		PlayerID:                       request.PlayerID,
		WalletID:                       request.WalletID,
		RoundID:                        request.RoundID,
		GameID:                         request.GameID,
		Kind:                           request.Kind,
		Amount:                         request.Money.Amount,
		Currency:                       request.Money.Currency,
		ReferenceExternalTransactionID: request.ReferenceExternalTransactionID,
		CorrelationID:                  correlationIDFromContext(r.Context()),
	})
	if err != nil {
		if errors.Is(err, ports.ErrConflict) {
			h.collector.ObserveConflict()
		}
		writeOperationError(w, r, h.logger, err)
		return
	}

	// Métricas de negócio (§12): resultado por tipo e contagem de duplicatas.
	// Um replay NÃO reaplica nada — medir isso separadamente é o que mostra se a
	// idempotência está sendo exercitada de fato.
	h.collector.ObserveOperation(request.Kind, result.State.String())
	if result.IdempotentReplay {
		h.collector.ObserveReplay()
	}

	response := operationResponse{
		TransactionID:    result.TransactionID.String(),
		Status:           result.State.String(),
		FailureCode:      result.FailureCode.String(),
		IdempotentReplay: result.IdempotentReplay,
	}
	// Só devolvemos saldo quando ele foi observado de fato.
	if result.BalanceKnown {
		balance := result.Balance
		response.Balance = &balance
	}

	// 202 quando a operação está REGISTRADA e aguardando referência (§7); 200
	// quando o processamento já produziu um resultado definitivo.
	status := http.StatusOK
	if result.State == transaction.StatePendingReference {
		status = http.StatusAccepted
	}

	writeJSON(w, status, response)
}

// -----------------------------------------------------------------------------
// Leitura
// -----------------------------------------------------------------------------

// handleGetWallet trata GET /wallets/{walletId}.
func (h *Handlers) handleGetWallet(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	result, err := h.getWallet.Execute(r.Context(), ports.GetWalletQuery{
		Principal: principal,
		WalletID:  r.PathValue("walletId"),
	})
	if err != nil {
		writeOperationError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusOK, walletResponse{
		WalletID: result.WalletID.String(),
		PlayerID: result.PlayerID.String(),
		Balance:  result.Balance,
		Version:  result.Version,
	})
}

// handleGetLedger trata GET /wallets/{walletId}/ledger.
func (h *Handlers) handleGetLedger(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	// O cursor chega como texto opaco e é decodificado aqui: o cliente nunca vê o
	// número, o que permite mudar a estratégia de paginação sem quebrar contrato.
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}

	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", "limit must be a non-negative integer")
			return
		}
		limit = parsed
	}

	page, err := h.getLedger.Execute(r.Context(), ports.GetLedgerQuery{
		Principal: principal,
		WalletID:  r.PathValue("walletId"),
		Cursor:    cursor,
		Limit:     limit,
	})
	if err != nil {
		writeOperationError(w, r, h.logger, err)
		return
	}

	entries := make([]ledgerEntryResponse, 0, len(page.Entries))
	for _, entry := range page.Entries {
		entries = append(entries, ledgerEntryResponse{
			ID:            entry.ID,
			TransactionID: entry.TransactionID.String(),
			Direction:     entry.Direction.String(),
			Amount:        entry.Amount,
			BalanceBefore: entry.BalanceBefore,
			BalanceAfter:  entry.BalanceAfter,
			CreatedAt:     entry.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, ledgerResponse{
		Entries:    entries,
		NextCursor: encodeCursor(page.NextCursor),
	})
}

// handleGetTransaction trata GET /wagering/transactions/{transactionId}.
func (h *Handlers) handleGetTransaction(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	result, err := h.getTransaction.Execute(r.Context(), ports.GetTransactionQuery{
		Principal:     principal,
		TransactionID: r.PathValue("transactionId"),
	})
	if err != nil {
		writeOperationError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusOK, toTransactionResponse(result))
}

// handleGetTransactionByExternalID trata
// GET /providers/{providerId}/wagering/transactions/{externalTransactionId}.
func (h *Handlers) handleGetTransactionByExternalID(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	result, err := h.getByExternal.Execute(r.Context(), ports.GetTransactionByExternalIDQuery{
		Principal: principal,
		// O `providerId` vem da ROTA; a validação contra a identidade acontece
		// no caso de uso (§2) — a URL nunca é fonte de autoridade.
		ProviderID:            r.PathValue("providerId"),
		ExternalTransactionID: r.PathValue("externalTransactionId"),
	})
	if err != nil {
		writeOperationError(w, r, h.logger, err)
		return
	}

	writeJSON(w, http.StatusOK, toTransactionResponse(result))
}

// -----------------------------------------------------------------------------
// POST /wallets/{walletId}/reconciliation
// -----------------------------------------------------------------------------

// handleReconcileWallet trata POST /wallets/{walletId}/reconciliation.
func (h *Handlers) handleReconcileWallet(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing credentials")
		return
	}

	result, err := h.reconcile.Execute(r.Context(), ports.ReconcileWalletQuery{
		Principal: principal,
		WalletID:  r.PathValue("walletId"),
	})
	if err != nil {
		writeOperationError(w, r, h.logger, err)
		return
	}

	// Divergência é reportada (resposta + log) e nunca corrigida aqui (§9). É o
	// sintoma mais grave do serviço: saldo armazenado != ledger. Vira métrica E log de
	// erro, para disparar alerta (§12).
	if !result.Consistent {
		h.collector.ObserveReconciliationDivergence()

		h.logger.Error("divergencia de reconciliacao",
			slog.String("wallet_id", result.WalletID.String()),
			slog.String("stored_balance", result.StoredBalance.String()),
			slog.String("calculated_balance", result.CalculatedBalance.String()),
			slog.String("difference", result.Difference.String()),
			slog.Int("checked_entries", result.CheckedEntries),
			slog.String("correlation_id", correlationIDFromContext(r.Context())),
		)
	}

	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID:          result.WalletID.String(),
		StoredBalance:     result.StoredBalance,
		CalculatedBalance: result.CalculatedBalance,
		Difference:        result.Difference,
		Consistent:        result.Consistent,
		CheckedEntries:    result.CheckedEntries,
	})
}

// decodeJSON exige campos conhecidos (um typo silencioso mudaria o hash de
// idempotência), corpo dentro do limite e um único objeto JSON.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	const maxBodyBytes = 1 << 20 // MaxBytesReader erra em vez de truncar em silêncio

	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
		}
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return err
	}

	// Segundo Decode deve bater em io.EOF: garante um único objeto.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

// encodeCursor converte o id do próximo lançamento em cursor opaco (base64 do inteiro):
// o cliente não infere a estrutura nem adivinha cursores. Cursor zero vira string vazia.
func encodeCursor(id int64) string {
	if id <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil // primeira página
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, errors.New("cursor is malformed")
	}
	id, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil || id < 0 {
		return 0, errors.New("cursor is malformed")
	}
	return id, nil
}

// toTransactionResponse projeta a view da aplicação para o JSON do §9.
func toTransactionResponse(view ports.TransactionView) transactionResponse {
	response := transactionResponse{
		TransactionID:                  view.TransactionID.String(),
		ProviderID:                     view.ProviderID.String(),
		ExternalTransactionID:          view.ExternalTransactionID.String(),
		PlayerID:                       view.PlayerID.String(),
		WalletID:                       view.WalletID.String(),
		RoundID:                        view.RoundID,
		GameID:                         view.GameID,
		Kind:                           view.Kind.String(),
		Status:                         view.State.String(),
		Money:                          view.Amount,
		ReferenceExternalTransactionID: view.ReferenceExternalID.String(),
		FailureCode:                    view.FailureCode.String(),
		Correctable:                    view.Correctable,
		Attempts:                       view.Attempts,
		CreatedAt:                      view.CreatedAt,
		UpdatedAt:                      view.UpdatedAt,
	}

	if !view.NextRetryAt.IsZero() {
		next := view.NextRetryAt
		response.NextRetryAt = &next
	}
	if !view.ProcessedAt.IsZero() {
		processed := view.ProcessedAt
		response.ProcessedAt = &processed
	}
	if view.HasResultBalance {
		balance := view.ResultBalance
		response.Balance = &balance
	}

	return response
}
