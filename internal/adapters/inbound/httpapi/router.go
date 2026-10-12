package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/metrics"
)

// HealthChecker é uma dependência verificável pelo readiness (§12). Declarada no
// consumidor: a API só precisa saber se "está saudável", não como se verifica.
type HealthChecker interface {
	// Name identifica a dependência na resposta do health check.
	Name() string
	// Check devolve erro quando a dependência está indisponível.
	Check(ctx context.Context) error
}

// RouterDeps reúne tudo que o roteador precisa.
type RouterDeps struct {
	Handlers    *Handlers
	Verifier    TokenVerifier
	IDGenerator ports.IDGenerator
	Logger      *slog.Logger
	Metrics     *metrics.Metrics
	// HealthChecks são checados no `/health/ready`.
	HealthChecks []HealthChecker
}

func NewRouter(deps RouterDeps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// --- Rotas PROTEGIDAS (exigem Bearer token) ---------------------------
	protected := http.NewServeMux()

	protected.HandleFunc("POST /wallets", deps.Handlers.handleOpenWallet)
	protected.HandleFunc("GET /wallets/{walletId}", deps.Handlers.handleGetWallet)
	protected.HandleFunc("GET /wallets/{walletId}/ledger", deps.Handlers.handleGetLedger)
	protected.HandleFunc("POST /wallets/{walletId}/reconciliation", deps.Handlers.handleReconcileWallet)
	protected.HandleFunc("POST /wagering/transactions", deps.Handlers.handlePlaceOperation)
	protected.HandleFunc("GET /wagering/transactions/{transactionId}", deps.Handlers.handleGetTransaction)
	protected.HandleFunc("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}",
		deps.Handlers.handleGetTransactionByExternalID)

	// --- Rotas PÚBLICAS ---------------------------------------------------
	root := http.NewServeMux()
	root.HandleFunc("GET /health/live", liveness)
	root.HandleFunc("GET /health/ready", readiness(deps.HealthChecks))

	// Tudo que não for health passa por autenticação. `/health/*` é mais específico que
	// "/", então continua público (um orquestrador precisa distinguir "processo morreu"
	// de "IdP caiu" — ações opostas).
	root.Handle("/", withAuth(deps.Verifier, logger, protected))

	// metrics é o penúltimo: conta tudo, inclusive as recusas por autenticação (taxa de
	// 401 subindo é sinal de segurança ou cliente mal configurado — precisa virar métrica).
	return withRecovery(logger, withCorrelation(deps.IDGenerator,
		withLogging(logger, withMetrics(deps.Metrics, root))))
}

// liveness responde se o processo está vivo (não checa dependências de propósito:
// reiniciar por banco fora não resolve e ainda derruba operações em andamento).
func liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// readiness responde se o processo pode receber tráfego — aqui dependências importam:
// sem banco ou broker a instância não conclui operações e deve sair da rotação.
func readiness(checks []HealthChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Timeout próprio: uma dependência pendurada não pode segurar o health check.
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		results := make(map[string]string, len(checks))
		healthy := true

		for _, check := range checks {
			if err := check.Check(ctx); err != nil {
				results[check.Name()] = "unavailable: " + err.Error()
				healthy = false
				continue
			}
			results[check.Name()] = "ok"
		}

		status := http.StatusOK
		state := "ready"
		if !healthy {
			status = http.StatusServiceUnavailable
			state = "degraded"
		}

		writeJSON(w, status, map[string]any{
			"status":       state,
			"dependencies": results,
		})
	}
}
