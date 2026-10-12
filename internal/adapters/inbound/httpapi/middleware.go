// Ordem dos middlewares (mais externo -> mais interno):
// recover -> correlation -> log -> metrics -> auth -> handler.
//
// recover primeiro (panic vira 500 controlado); correlation antes do log (o log já
// tem o id); auth depois de log/metrics (tentativa negada é registrada e contada) e
// antes do handler (nenhum handler precisa lembrar de checar token).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/outbound/keycloak"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/metrics"
)

// contextKey é um tipo privado: string crua como chave permitiria a dois pacotes
// colidirem e sobrescreverem o valor um do outro.
type contextKey int

const (
	principalKey contextKey = iota
	correlationIDKey
)

func principalFromContext(ctx context.Context) (ports.Principal, bool) {
	principal, ok := ctx.Value(principalKey).(ports.Principal)
	return principal, ok
}

func correlationIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(correlationIDKey).(string)
	return value
}

// TokenVerifier é declarada no consumidor (não no adapter do Keycloak): define o
// mínimo necessário e permite injetar um verificador falso nos testes.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (ports.Principal, error)
}

// withRecovery converte panic em 500 e mantém o processo de pé.
func withRecovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Recorder para não reescrever o cabeçalho: um panic após WriteHeader não pode
		// emitir o 500 de novo.
		recorder := &statusRecorder{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				logger.Error("panic no handler",
					slog.Any("panic", p),
					slog.String("correlation_id", correlationIDFromContext(r.Context())),
					slog.String("path", r.URL.Path),
				)
				if !recorder.wrote {
					writeJSONError(recorder, http.StatusInternalServerError, "internal_error", "unexpected server error")
				}
			}
		}()
		next.ServeHTTP(recorder, r)
	})
}

// withCorrelation garante um identificador de rastreio por requisição: preserva o
// `X-Correlation-ID` do cliente ou gera um, para o rastro HTTP -> outbox -> SQS ->
// worker (§12).
func withCorrelation(idGenerator ports.IDGenerator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))

		if correlationID == "" {
			generated, err := idGenerator.NewID(r.Context())
			if err != nil {
				// Sem id não conseguimos rastrear; ainda assim atendemos, porque
				// falhar aqui tiraria a API do ar por um detalhe de rastreio.
				correlationID = "unavailable"
			} else {
				correlationID = generated
			}
		}

		ctx := context.WithValue(r.Context(), correlationIDKey, correlationID)
		// Devolvemos o id no cabeçalho: o cliente consegue reportar um problema
		// citando exatamente o identificador que aparece nos nossos logs.
		w.Header().Set("X-Correlation-ID", correlationID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captura o status e marca se a resposta já começou (evita segundo
// WriteHeader).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.status = status
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// withLogging registra cada requisição com os identificadores do §12.
func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}

		// Nível por resultado: 5xx é erro (precisa de ação), 4xx é aviso (o
		// cliente errou), 2xx/3xx é informativo. Isso mantém o alerta útil.
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case status >= 400:
			level = slog.LevelWarn
		}

		logger.Log(r.Context(), level, "requisicao http",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
			slog.String("correlation_id", correlationIDFromContext(r.Context())),
			// NÃO registramos corpo, cabeçalhos de autorização nem payload
			// financeiro completo (§12: "não registre credenciais, dados
			// sensíveis ou payloads financeiros completos").
		)
	})
}

// withMetrics usa o PADRÃO da rota (`r.Pattern`, Go 1.22+), não o caminho concreto:
// com o caminho real, cada wallet viraria uma série nova e as métricas cresceriam sem
// limite.
func withMetrics(collector *metrics.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}

		route := r.Pattern
		if route == "" {
			// Rota não reconhecida (404 antes do mux casar): agrupamos para não
			// criar cardinalidade a partir de caminhos arbitrários de scanner.
			route = "unmatched"
		}
		collector.ObserveHTTP(r.Method, route, status, time.Since(start))
	})
}

// withAuth valida o Bearer token e injeta a identidade no contexto.
func withAuth(verifier TokenVerifier, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawToken, err := bearerToken(r)
		if err != nil {
			// 401 sem detalhe: não informamos se o token está expirado, se a
			// assinatura é inválida ou se o emissor difere.
			w.Header().Set("WWW-Authenticate", `Bearer realm="wager"`)
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid credentials")
			return
		}

		principal, err := verifier.Verify(r.Context(), rawToken)
		if err != nil {
			logger.Warn("autenticacao recusada",
				slog.String("correlation_id", correlationIDFromContext(r.Context())),
				slog.String("error", err.Error()),
			)

			// Falha de INFRAESTRUTURA (JWKS inacessível) NÃO é "não autorizado":
			// é indisponibilidade. Devolver 401 faria o cliente achar que a
			// credencial é o problema e trocá-la em vão.
			if errors.Is(err, ports.ErrUnavailable) {
				writeJSONError(w, http.StatusServiceUnavailable, "unavailable", "authentication service unavailable")
				return
			}

			w.Header().Set("WWW-Authenticate", `Bearer realm="wager"`)
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid credentials")
			return
		}

		ctx := context.WithValue(r.Context(), principalKey, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerToken extrai o token do cabeçalho Authorization.
func bearerToken(r *http.Request) (string, error) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return "", keycloak.ErrUnauthenticated
	}

	// `CutPrefix` devolve o resto e um booleano — mais claro que `strings.Split`,
	// que aceitaria "Bearer" solto ou com espaços extras.
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found {
		return "", keycloak.ErrUnauthenticated
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", keycloak.ErrUnauthenticated
	}
	return token, nil
}

// errorResponse é o envelope estável de erro: o cliente trata falhas pelo código, sem
// interpretar texto humano.
type errorResponse struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	CorrelationID string `json:"correlationId,omitempty"`
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	// Os cabeçalhos já foram enviados; não há como trocar o status. O corpo parcial é
	// o melhor possível aqui.
	_ = json.NewEncoder(w).Encode(payload)
}

// mapErrorToStatus traduz os sentinelas da aplicação para status HTTP (§9). A tradução
// vive no adapter: status HTTP é detalhe de transporte.
func mapErrorToStatus(err error) (int, string) {
	switch {
	case errors.Is(err, ports.ErrValidation):
		// 422: a requisição é sintaticamente válida, mas o conteúdo não passa na
		// regra de negócio de entrada.
		return http.StatusUnprocessableEntity, "validation_error"

	case errors.Is(err, ports.ErrForbidden):
		return http.StatusForbidden, "forbidden"

	case errors.Is(err, ports.ErrNotFound):
		return http.StatusNotFound, "not_found"

	case errors.Is(err, ports.ErrConflict):
		return http.StatusConflict, "conflict"

	case errors.Is(err, ports.ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable"

	default:
		return http.StatusInternalServerError, "internal_error"
	}
}

func writeOperationError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	status, code := mapErrorToStatus(err)

	if status >= 500 {
		logger.Error("erro ao processar requisicao",
			slog.String("correlation_id", correlationIDFromContext(r.Context())),
			slog.String("error", err.Error()),
		)
	}

	// A mensagem devolvida é GENÉRICA para 5xx: detalhes internos não vazam.
	message := err.Error()
	if status >= 500 {
		message = "unexpected server error"
	}

	writeJSONError(w, status, code, message)
}
