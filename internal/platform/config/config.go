// Configuração via ambiente (12-factor), validada na partida (§4): se falta variável
// obrigatória, o processo não sobe — em vez de falhar só na primeira requisição.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config reúne tudo que o processo precisa para subir.
type Config struct {
	HTTPAddr string

	DatabaseURL      string
	DatabaseMaxConns int32

	SQSEndpoint          string // vazio = endpoint real da AWS
	SQSRegion            string
	SQSQueueURL          string // fila de entrada (wager-transactions.fifo)
	SQSEventsQueueURL    string // destino dos eventos (wager-events.fifo)
	SQSConsumerName      string // usado na chave da inbox
	SQSMaxMessages       int
	SQSWaitTimeSeconds   int
	SQSVisibilityTimeout int

	KeycloakIssuer  string
	KeycloakJWKSURL string

	LogLevel    string
	MetricsAddr string

	// Timeouts do servidor HTTP (ReadHeaderTimeout é fixo no servidor).
	HTTPReadTimeout  time.Duration
	HTTPWriteTimeout time.Duration
	HTTPIdleTimeout  time.Duration

	ShutdownTimeout time.Duration

	PendingReferenceBaseBackoff  time.Duration
	PendingReferenceMaxAttempts  int
	PendingReferenceScanInterval time.Duration
	OutboxRelayInterval          time.Duration
	OutboxRelayBatchSize         int
	OutboxRelayLease             time.Duration
	OutboxRelayMaxAttempts       int
}

// Load lê o ambiente, aplica defaults e valida. Valor numérico ou de duração inválido
// falha a partida em vez de cair no default em silêncio.
func Load() (Config, error) {
	var errs []error

	intEnv := func(key string, fallback int) int {
		value, err := envIntStrict(key, fallback)
		if err != nil {
			errs = append(errs, err)
		}
		return value
	}
	durationEnv := func(key string, fallback time.Duration) time.Duration {
		value, err := envDurationStrict(key, fallback)
		if err != nil {
			errs = append(errs, err)
		}
		return value
	}

	cfg := Config{
		HTTPAddr: envOr("HTTP_ADDR", ":8080"),

		DatabaseURL:      os.Getenv("DATABASE_URL"),
		DatabaseMaxConns: int32(intEnv("DATABASE_MAX_CONNS", 20)),

		SQSEndpoint:          os.Getenv("AWS_ENDPOINT_URL"),
		SQSRegion:            envOr("AWS_REGION", "us-east-1"),
		SQSQueueURL:          os.Getenv("SQS_TRANSACTIONS_QUEUE_URL"),
		SQSEventsQueueURL:    os.Getenv("SQS_EVENTS_QUEUE_URL"),
		SQSConsumerName:      envOr("SQS_CONSUMER_NAME", "wager-transactions-worker"),
		SQSMaxMessages:       intEnv("SQS_MAX_MESSAGES", 10),
		SQSWaitTimeSeconds:   intEnv("SQS_WAIT_TIME_SECONDS", 20),
		SQSVisibilityTimeout: intEnv("SQS_VISIBILITY_TIMEOUT", 30),

		KeycloakIssuer:  os.Getenv("KEYCLOAK_ISSUER"),
		KeycloakJWKSURL: os.Getenv("KEYCLOAK_JWKS_URL"),

		LogLevel:         envOr("LOG_LEVEL", "info"),
		MetricsAddr:      envOr("METRICS_ADDR", ":9090"),
		HTTPReadTimeout:  durationEnv("HTTP_READ_TIMEOUT", 15*time.Second),
		HTTPWriteTimeout: durationEnv("HTTP_WRITE_TIMEOUT", 30*time.Second),
		HTTPIdleTimeout:  durationEnv("HTTP_IDLE_TIMEOUT", 60*time.Second),

		ShutdownTimeout: durationEnv("SHUTDOWN_TIMEOUT", 20*time.Second),

		PendingReferenceBaseBackoff:  durationEnv("PENDING_REFERENCE_BASE_BACKOFF", 2*time.Second),
		PendingReferenceMaxAttempts:  intEnv("PENDING_REFERENCE_MAX_ATTEMPTS", 8),
		PendingReferenceScanInterval: durationEnv("PENDING_REFERENCE_SCAN_INTERVAL", 5*time.Second),
		OutboxRelayInterval:          durationEnv("OUTBOX_RELAY_INTERVAL", time.Second),
		OutboxRelayBatchSize:         intEnv("OUTBOX_RELAY_BATCH_SIZE", 20),
		OutboxRelayLease:             durationEnv("OUTBOX_RELAY_LEASE", 30*time.Second),
		OutboxRelayMaxAttempts:       intEnv("OUTBOX_RELAY_MAX_ATTEMPTS", 10),
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: valores invalidos: %w", errors.Join(errs...))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate acumula todos os problemas antes de falhar: listar tudo de uma vez evita o
// ciclo "corrige, sobe, descobre o próximo".
func (c Config) validate() error {
	var missing []string

	if strings.TrimSpace(c.DatabaseURL) == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if strings.TrimSpace(c.SQSQueueURL) == "" {
		missing = append(missing, "SQS_TRANSACTIONS_QUEUE_URL")
	}
	if strings.TrimSpace(c.SQSEventsQueueURL) == "" {
		missing = append(missing, "SQS_EVENTS_QUEUE_URL")
	}
	// As variáveis do Keycloak não são obrigatórias aqui de propósito: o worker não
	// atende HTTP e não valida token (§2). Quem exige é NewTokenVerifier, só no processo
	// da API.

	if len(missing) > 0 {
		return fmt.Errorf("config: variaveis de ambiente obrigatorias ausentes: %s (veja .env.example)",
			strings.Join(missing, ", "))
	}

	if c.SQSMaxMessages < 1 || c.SQSMaxMessages > 10 {
		// A própria SQS limita a 10 mensagens por ReceiveMessage.
		return fmt.Errorf("config: SQS_MAX_MESSAGES deve estar entre 1 e 10, got %d", c.SQSMaxMessages)
	}
	if c.SQSWaitTimeSeconds < 0 || c.SQSWaitTimeSeconds > 20 {
		// Long polling: 0 = short polling (busy loop), máximo 20s.
		return fmt.Errorf("config: SQS_WAIT_TIME_SECONDS deve estar entre 0 e 20, got %d", c.SQSWaitTimeSeconds)
	}
	if c.SQSVisibilityTimeout <= 0 {
		return fmt.Errorf("config: SQS_VISIBILITY_TIMEOUT deve ser positivo")
	}

	return nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// envIntStrict devolve o fallback se ausente e erro se presente e malformado.
func envIntStrict(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback, fmt.Errorf("%s deve ser um inteiro, got %q", key, raw)
	}
	return value, nil
}

// envDurationStrict: idem para durações (ex.: "5s", "1m").
func envDurationStrict(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback, fmt.Errorf("%s deve ser uma duracao (ex.: 5s), got %q", key, raw)
	}
	return value, nil
}
