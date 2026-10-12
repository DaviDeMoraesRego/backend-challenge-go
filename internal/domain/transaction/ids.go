package transaction

import "fmt"

var (
	ErrInvalidID             = fmt.Errorf("transaction: invalid id")
	ErrInvalidProvider       = fmt.Errorf("transaction: invalid provider id")
	ErrInvalidExternalID     = fmt.Errorf("transaction: invalid external transaction id")
	ErrInvalidIdempotencyKey = fmt.Errorf("transaction: invalid idempotency key")

	// ErrInvalidPayloadHash: hash ausente ou fora do formato esperado.
	ErrInvalidPayloadHash = fmt.Errorf("transaction: invalid payload hash")
)

// ID é a identidade interna e estável da transação (§6.3); aparece nos eventos como
// `aggregateId`/`transactionId`.
type ID string

func NewID(raw string) (ID, error) {
	if isBlank(raw) {
		return "", fmt.Errorf("%w: must not be empty", ErrInvalidID)
	}
	return ID(raw), nil
}

func (id ID) String() string { return string(id) }

// ProviderID identifica o provedor autorizado (§2). Não é um ID arbitrário do
// cliente: é derivado da identidade autenticada (claim do token), o que garante o
// isolamento entre provedores.
type ProviderID string

func NewProviderID(raw string) (ProviderID, error) {
	if isBlank(raw) {
		return "", fmt.Errorf("%w: must not be empty", ErrInvalidProvider)
	}
	return ProviderID(raw), nil
}

func (p ProviderID) String() string { return string(p) }

// ExternalTransactionID é o identificador da operação no provedor. Com o providerId
// forma a chave natural (providerId, externalId) única no schema (§6.3/§9).
type ExternalTransactionID string

func NewExternalTransactionID(raw string) (ExternalTransactionID, error) {
	if isBlank(raw) {
		return "", fmt.Errorf("%w: must not be empty", ErrInvalidExternalID)
	}
	return ExternalTransactionID(raw), nil
}

func (e ExternalTransactionID) String() string { return string(e) }

// IdempotencyKey é a chave do header `Idempotency-Key` (§9). O servidor a armazena
// como veio: não substitui silenciosamente por uma chave calculada.
type IdempotencyKey string

func NewIdempotencyKey(raw string) (IdempotencyKey, error) {
	if isBlank(raw) {
		return "", fmt.Errorf("%w: must not be empty", ErrInvalidIdempotencyKey)
	}
	return IdempotencyKey(raw), nil
}

func (k IdempotencyKey) String() string { return string(k) }

// PayloadHash é o hash determinístico dos campos de negócio (§9), em hexadecimal
// minúsculo (encoding/hex sobre SHA-256). Distingue replay (mesmo hash) de conflito
// (hash diferente para a mesma chave).
type PayloadHash string

// payloadHashHexLen: SHA-256 em hexadecimal (32 bytes * 2).
const payloadHashHexLen = 64

// NewPayloadHash valida o hash: 64 caracteres hexadecimais minúsculos.
func NewPayloadHash(raw string) (PayloadHash, error) {
	if len(raw) != payloadHashHexLen {
		return "", fmt.Errorf("%w: expected %d hex chars, got %d", ErrInvalidPayloadHash, payloadHashHexLen, len(raw))
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return "", fmt.Errorf("%w: %q is not lowercase hexadecimal", ErrInvalidPayloadHash, raw)
		}
	}
	return PayloadHash(raw), nil
}

func (h PayloadHash) String() string { return string(h) }

// isBlank duplicado de propósito: um pacote compartilhado criaria acoplamento
// artificial entre agregados independentes.
func isBlank(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return false
		}
	}
	return true
}
