// Logger estruturado com log/slog (stdlib, Go 1.21+): níveis, campos tipados e saída
// JSON. Em JSON os registros são CONSULTÁVEIS por campo (§12: correlationId, messageId,
// transactionId, walletId, providerId) — diferente de texto puro.
package observability

import (
	"log/slog"
	"os"
	"strings"
)

// LoggerConfig parametriza o logger.
type LoggerConfig struct {
	// Level: "debug", "info", "warn" ou "error".
	Level string
	// ServiceName aparece em todo registro, identificando a origem.
	ServiceName string
	// Pretty imprime texto legível em vez de JSON (útil em desenvolvimento).
	Pretty bool
}

// NewLogger cria o logger estruturado.
func NewLogger(cfg LoggerConfig) *slog.Logger {
	options := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}

	var handler slog.Handler
	if cfg.Pretty {
		handler = slog.NewTextHandler(os.Stdout, options)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, options)
	}

	logger := slog.New(handler)
	if cfg.ServiceName != "" {
		// With devolve um logger derivado: o nome do serviço não se repete em cada chamada.
		logger = logger.With(slog.String("service", cfg.ServiceName))
	}
	return logger
}

// parseLevel traduz o texto para slog.Level. Nível desconhecido cai em info em vez de
// derrubar a aplicação por um valor cosmético.
func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
