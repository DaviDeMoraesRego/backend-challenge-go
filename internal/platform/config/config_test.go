package config

import (
	"strings"
	"testing"
)

// setRequired define o mínimo obrigatório para um Load válido.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost/db")
	t.Setenv("SQS_TRANSACTIONS_QUEUE_URL", "http://localhost/q")
	t.Setenv("SQS_EVENTS_QUEUE_URL", "http://localhost/e")
}

func TestLoad_ValidDefaults(t *testing.T) {
	setRequired(t)
	t.Setenv("SQS_MAX_MESSAGES", "") // ausente -> default

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SQSMaxMessages != 10 {
		t.Errorf("SQSMaxMessages = %d, want 10", cfg.SQSMaxMessages)
	}
	if cfg.HTTPReadTimeout <= 0 || cfg.HTTPWriteTimeout <= 0 || cfg.HTTPIdleTimeout <= 0 {
		t.Errorf("timeouts HTTP nao definidos: %+v", cfg)
	}
}

// TestLoad_InvalidIntFailsFast: inteiro malformado falha em vez de virar default.
func TestLoad_InvalidIntFailsFast(t *testing.T) {
	setRequired(t)
	t.Setenv("SQS_MAX_MESSAGES", "abc")

	_, err := Load()
	if err == nil {
		t.Fatal("esperava erro para SQS_MAX_MESSAGES invalido")
	}
	if !strings.Contains(err.Error(), "SQS_MAX_MESSAGES") {
		t.Errorf("erro nao cita a variavel: %v", err)
	}
}

func TestLoad_InvalidDurationFailsFast(t *testing.T) {
	setRequired(t)
	t.Setenv("SHUTDOWN_TIMEOUT", "nao-e-duracao")

	if _, err := Load(); err == nil {
		t.Fatal("esperava erro para SHUTDOWN_TIMEOUT invalido")
	}
}

func TestLoad_MissingRequiredFails(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("SQS_TRANSACTIONS_QUEUE_URL", "")
	t.Setenv("SQS_EVENTS_QUEUE_URL", "")

	_, err := Load()
	if err == nil {
		t.Fatal("esperava erro por variaveis obrigatorias ausentes")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("erro nao lista a variavel ausente: %v", err)
	}
}
