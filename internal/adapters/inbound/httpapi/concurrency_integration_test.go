//go:build integration

// Bateria de concorrência do §13 contra o sistema real (HTTP -> use case -> Postgres),
// com requisições disparadas em paralelo. Rodar com:
//
//	go test -tags=integration -race ./internal/adapters/inbound/httpapi/ -v
//
// Requer o Postgres do docker compose (ou TEST_DATABASE_URL). O que está sob teste é a
// coordenação entre requisições simultâneas (locks de linha, constraints únicas,
// releitura pós-lock) — impossível de provar com fakes. A autenticação é falsa de
// propósito; a real é exercitada pelo teste de fumaça.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/inbound/httpapi"
	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/outbound/postgres"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/usecase"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/clock"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/idgen"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/metrics"
)

const defaultTestDatabaseURL = "postgres://wager:wager@localhost:5432/wager?sslmode=disable"

// fakeVerifier mapeia tokens literais para identidades. Ver a nota no cabeçalho.
type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string) (ports.Principal, error) {
	switch token {
	case "internal":
		return ports.Principal{Scope: ports.ScopeInternal}, nil
	case "provider-a":
		return ports.Principal{Scope: ports.ScopeProvider, ProviderID: "provider-a"}, nil
	default:
		return ports.Principal{}, errors.New("invalid token")
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		url = defaultTestDatabaseURL
	}

	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{URL: url, MaxConns: 25})
	if err != nil {
		t.Fatalf("nao foi possivel conectar em %s: %v (suba o ambiente com docker compose up -d)", url, err)
	}
	t.Cleanup(pool.Close)

	resetDatabase(t, pool)

	uow := postgres.NewUnitOfWork(pool)
	clk := clock.New()
	ids := idgen.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	handlers := httpapi.NewHandlers(
		usecase.NewOpenWalletService(uow, clk, ids),
		usecase.NewPlaceWagerOperationService(uow, clk, ids, usecase.PlaceWagerOperationConfig{
			PendingReferenceBaseBackoff: time.Second,
		}),
		usecase.NewGetWalletService(uow),
		usecase.NewGetWalletLedgerService(uow),
		usecase.NewGetWagerTransactionService(uow),
		usecase.NewGetWagerTransactionByExternalIDService(uow),
		usecase.NewReconcileWalletService(uow),
		logger,
		metrics.New(),
	)

	router := httpapi.NewRouter(httpapi.RouterDeps{
		Handlers:    handlers,
		Verifier:    fakeVerifier{},
		IDGenerator: ids,
		Logger:      logger,
		Metrics:     metrics.New(),
	})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

// resetDatabase desabilita os triggers de imutabilidade e trunca todas as tabelas num
// único comando (sem CASCADE, que propagaria ao ledger protegido).
func resetDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	statements := []string{
		`ALTER TABLE wallet_ledger_entries DISABLE TRIGGER USER`,
		`TRUNCATE wallet_ledger_entries, outbox_events, inbox_messages, wager_transactions, wallets RESTART IDENTITY`,
		`ALTER TABLE wallet_ledger_entries ENABLE TRIGGER USER`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("reset falhou em %q: %v", statement, err)
		}
	}
}

type apiClient struct {
	server *httptest.Server
}

func (c apiClient) do(t *testing.T, method, path, token, body, idempotencyKey string) (int, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}

	request, err := http.NewRequest(method, c.server.URL+path, reader)
	if err != nil {
		t.Fatalf("nao foi possivel montar a requisicao: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}

	response, err := c.server.Client().Do(request)
	if err != nil {
		t.Fatalf("requisicao falhou: %v", err)
	}
	defer response.Body.Close()

	raw, _ := io.ReadAll(response.Body)
	decoded := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return response.StatusCode, decoded
}

// openWallet abre uma carteira pelo endpoint interno.
func (c apiClient) openWallet(t *testing.T, playerID, amount string) string {
	t.Helper()

	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, playerID, amount)
	status, response := c.do(t, http.MethodPost, "/wallets", "internal", body, "")
	if status != http.StatusCreated {
		t.Fatalf("abertura de carteira devolveu %d: %v", status, response)
	}
	return response["id"].(string)
}

// betBody monta o corpo de uma operação.
func betBody(playerID, walletID, externalID, amount string) string {
	return fmt.Sprintf(`{
		"providerId":"provider-a",
		"externalTransactionId":%q,
		"playerId":%q,
		"walletId":%q,
		"roundId":"round-987",
		"gameId":"fortune-chimp",
		"kind":"BET",
		"money":{"amount":%q,"currency":"BRL"}
	}`, externalID, playerID, walletID, amount)
}

// TestConcurrentDuplicateBets (§13.1): a mesma operação (mesma chave) 50 vezes em
// paralelo; apenas uma movimenta o saldo e as demais viram replay. Prova a idempotência
// persistente sob corrida (as perdedoras perdem o índice único e releem o vencedor).
func TestConcurrentDuplicateBets(t *testing.T) {
	server := newTestServer(t)
	client := apiClient{server: server}

	playerID := "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	walletID := client.openWallet(t, playerID, "100.00")

	const attempts = 50

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		processed int
		replays   int
		failures  []string
	)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			status, response := client.do(t, http.MethodPost, "/wagering/transactions", "provider-a",
				betBody(playerID, walletID, "bet-race-1", "25.00"), "provider-a:bet-race-1")

			mu.Lock()
			defer mu.Unlock()

			if status != http.StatusOK {
				failures = append(failures, fmt.Sprintf("status %d: %v", status, response))
				return
			}
			if replay, ok := response["idempotentReplay"].(bool); ok && replay {
				replays++
				return
			}
			processed++
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("requisicoes falharam: %v", failures)
	}
	// Exatamente UMA aplicação real.
	if processed != 1 {
		t.Errorf("operacoes aplicadas = %d, want 1 (a idempotencia deve barrar as demais)", processed)
	}
	if replays != attempts-1 {
		t.Errorf("replays = %d, want %d", replays, attempts-1)
	}

	// O saldo comprova: 100.00 - 25.00, e não 100.00 - 25.00*50.
	_, wallet := client.do(t, http.MethodGet, "/wallets/"+walletID, "internal", "", "")
	balance := wallet["balance"].(map[string]any)["amount"].(string)
	if balance != "75.00" {
		t.Errorf("saldo = %s, want 75.00 (movimentacao duplicada detectada)", balance)
	}

	// E o ledger tem DOIS lançamentos: abertura + a única aposta efetivada.
	status, ledger := client.do(t, http.MethodGet, "/wallets/"+walletID+"/ledger", "internal", "", "")
	if status != http.StatusOK {
		t.Fatalf("extrato devolveu %d", status)
	}
	entries := ledger["entries"].([]any)
	if len(entries) != 2 {
		t.Errorf("lancamentos = %d, want 2 (abertura + uma unica aposta)", len(entries))
	}
}

// -----------------------------------------------------------------------------
// §8/§13.2 — A DISPUTA das duas apostas de 80.00 sobre saldo de 100.00
// -----------------------------------------------------------------------------

// TestConcurrentTwoBetsDispute reproduz, com CONCORRÊNCIA REAL, o cenário
// obrigatório do §8: duas apostas DIFERENTES de 80.00 disparadas ao mesmo tempo
// sobre uma carteira com 100.00.
//
// Resultado exigido: UMA processada, UMA rejeitada por saldo, saldo final de
// 20.00 e UM ÚNICO débito no ledger.
func TestConcurrentTwoBetsDispute(t *testing.T) {
	server := newTestServer(t)
	client := apiClient{server: server}

	playerID := "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a2"
	walletID := client.openWallet(t, playerID, "100.00")

	type outcome struct {
		status   string
		failure  string
		balance  string
		httpCode int
	}

	results := make([]outcome, 2)
	var wg sync.WaitGroup

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			externalID := fmt.Sprintf("bet-dispute-%d", index+1)
			httpStatus, response := client.do(t, http.MethodPost, "/wagering/transactions", "provider-a",
				betBody(playerID, walletID, externalID, "80.00"), "provider-a:"+externalID)

			result := outcome{httpCode: httpStatus}
			if state, ok := response["status"].(string); ok {
				result.status = state
			}
			if code, ok := response["failureCode"].(string); ok {
				result.failure = code
			}
			if balance, ok := response["balance"].(map[string]any); ok {
				result.balance = balance["amount"].(string)
			}
			results[index] = result
		}(i)
	}
	wg.Wait()

	var processed, rejected int
	for _, result := range results {
		switch result.status {
		case "PROCESSED":
			processed++
		case "REJECTED":
			rejected++
		}
	}

	if processed != 1 {
		t.Errorf("apostas processadas = %d, want 1 (resultados: %+v)", processed, results)
	}
	if rejected != 1 {
		t.Errorf("apostas rejeitadas = %d, want 1 (resultados: %+v)", rejected, results)
	}

	// A rejeição precisa ser por SALDO, com o código próprio de aposta (§7).
	for _, result := range results {
		if result.status == "REJECTED" && result.failure != "BET_INSUFFICIENT_FUNDS" {
			t.Errorf("failureCode = %q, want BET_INSUFFICIENT_FUNDS", result.failure)
		}
	}

	_, wallet := client.do(t, http.MethodGet, "/wallets/"+walletID, "internal", "", "")
	balance := wallet["balance"].(map[string]any)["amount"].(string)
	if balance != "20.00" {
		t.Errorf("saldo final = %s, want 20.00", balance)
	}

	_, ledger := client.do(t, http.MethodGet, "/wallets/"+walletID+"/ledger", "internal", "", "")
	if entries := ledger["entries"].([]any); len(entries) != 2 {
		t.Errorf("lancamentos = %d, want 2 (a rejeitada NAO gera ledger)", len(entries))
	}
}

// -----------------------------------------------------------------------------
// §13.3/§13.4 — Carteiras distintas em paralelo + consistência final
// -----------------------------------------------------------------------------

// TestConcurrentDistinctWallets prova o outro lado do §8: carteiras DIFERENTES
// avançam em PARALELO. O lock é por linha (§5.6 proíbe lock global), então
// operações de carteiras distintas não se bloqueiam.
func TestConcurrentDistinctWallets(t *testing.T) {
	server := newTestServer(t)
	client := apiClient{server: server}

	const wallets = 8

	walletIDs := make([]string, wallets)
	playerIDs := make([]string, wallets)
	for i := 0; i < wallets; i++ {
		playerIDs[i] = fmt.Sprintf("0192f28f-5dc0-7d58-bdb2-814ad6a0f%03d", i)
		walletIDs[i] = client.openWallet(t, playerIDs[i], "500.00")
	}

	var wg sync.WaitGroup
	failures := make(chan string, wallets)

	start := time.Now()
	for i := 0; i < wallets; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			externalID := fmt.Sprintf("bet-parallel-%d", index)
			status, response := client.do(t, http.MethodPost, "/wagering/transactions", "provider-a",
				betBody(playerIDs[index], walletIDs[index], externalID, "100.00"), "provider-a:"+externalID)

			if status != http.StatusOK || response["status"] != "PROCESSED" {
				failures <- fmt.Sprintf("carteira %d: status %d, resposta %v", index, status, response)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	elapsed := time.Since(start)

	for failure := range failures {
		t.Errorf("%s", failure)
	}

	// Cada carteira avançou com o seu próprio lock: todas processadas.
	for i := 0; i < wallets; i++ {
		_, wallet := client.do(t, http.MethodGet, "/wallets/"+walletIDs[i], "internal", "", "")
		balance := wallet["balance"].(map[string]any)["amount"].(string)
		if balance != "400.00" {
			t.Errorf("carteira %d: saldo = %s, want 400.00", i, balance)
		}
	}

	t.Logf("%d carteiras distintas processadas em paralelo em %s", wallets, elapsed)
}

// TestLedgerAlwaysMatchesStoredBalance fecha a bateria com a checagem que o §13
// exige ao final: o saldo armazenado tem de bater com a soma dos lançamentos.
func TestLedgerAlwaysMatchesStoredBalance(t *testing.T) {
	server := newTestServer(t)
	client := apiClient{server: server}

	playerID := "0192f28f-5dc0-7d58-bdb2-814ad6a0f4b1"
	walletID := client.openWallet(t, playerID, "1000.00")

	// Uma mistura de operações, incluindo as que NÃO movimentam a carteira.
	operations := []struct {
		kind   string
		amount string
		id     string
	}{
		{kind: "BET", amount: "100.00", id: "op-1"},
		{kind: "WIN", amount: "50.00", id: "op-2"},
		{kind: "LOSS", amount: "0.00", id: "op-3"},
		{kind: "BET", amount: "10.00", id: "op-4"},
	}

	for _, operation := range operations {
		body := fmt.Sprintf(`{
			"providerId":"provider-a",
			"externalTransactionId":%q,
			"playerId":%q,
			"walletId":%q,
			"roundId":"round-987",
			"gameId":"fortune-chimp",
			"kind":%q,
			"money":{"amount":%q,"currency":"BRL"}
		}`, operation.id, playerID, walletID, operation.kind, operation.amount)

		status, response := client.do(t, http.MethodPost, "/wagering/transactions", "provider-a", body, "provider-a:"+operation.id)
		if status != http.StatusOK {
			t.Fatalf("operacao %s devolveu %d: %v", operation.kind, status, response)
		}
		if response["status"] != "PROCESSED" {
			t.Fatalf("operacao %s nao foi processada: %v", operation.kind, response)
		}
	}

	status, reconciliation := client.do(t, http.MethodPost, "/wallets/"+walletID+"/reconciliation", "internal", "", "")
	if status != http.StatusOK {
		t.Fatalf("reconciliacao devolveu %d", status)
	}

	if consistent, _ := reconciliation["consistent"].(bool); !consistent {
		t.Errorf("reconciliacao inconsistente: %v", reconciliation)
	}
	// Abertura (1000) - 100 + 50 - 10 = 940.00. LOSS não movimenta.
	if calculated := reconciliation["calculatedBalance"].(map[string]any)["amount"].(string); calculated != "940.00" {
		t.Errorf("saldo reconstruido = %s, want 940.00", calculated)
	}
	if stored := reconciliation["storedBalance"].(map[string]any)["amount"].(string); stored != "940.00" {
		t.Errorf("saldo armazenado = %s, want 940.00", stored)
	}
	// checkedEntries: abertura + BET + WIN + BET. LOSS não gera lançamento (§6.4).
	if entries := reconciliation["checkedEntries"].(float64); entries != 4 {
		t.Errorf("checkedEntries = %v, want 4", entries)
	}
}

// TestMissingWalletReturns404: carteira inexistente é 404 e não persiste transação.
func TestMissingWalletReturns404(t *testing.T) {
	server := newTestServer(t)
	client := apiClient{server: server}

	body := betBody(
		"0192f28f-5dc0-7d58-bdb2-814ad6a0f4ff",
		"0192f291-27dd-7d3f-8071-5f8685deef00",
		"bet-fantasma",
		"10.00",
	)

	status, response := client.do(t, http.MethodPost, "/wagering/transactions", "provider-a",
		body, "provider-a:bet-fantasma")

	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%v)", status, response)
	}
}
