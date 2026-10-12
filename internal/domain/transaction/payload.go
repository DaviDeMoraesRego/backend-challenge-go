package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/money"
	"github.com/davidemoraesrego/backend-challenge-go/internal/domain/wallet"
)

// CanonicalPayloadFields reúne os campos de negócio que entram no hash. Fora dele,
// por exigência do §9: idempotencyKey, metadados de transporte (messageId,
// correlationId) e timestamps.
type CanonicalPayloadFields struct {
	ProviderID                     ProviderID
	ExternalTransactionID          ExternalTransactionID
	PlayerID                       wallet.PlayerID
	WalletID                       wallet.ID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Amount                         money.Money
	ReferenceExternalTransactionID ExternalTransactionID
}

// canonicalPayload é a projeção serializável do payload de negócio. As tags estão em
// ordem alfabética de propósito: `encoding/json` serializa structs na ordem de
// declaração, e reordenar mudaria o hash (protegido por teste golden). `Amount` é
// string porque dinheiro nunca deve ser número em trânsito (§5.1).
type canonicalPayload struct {
	Amount                         string `json:"amount"`
	Currency                       string `json:"currency"`
	ExternalTransactionID          string `json:"externalTransactionId"`
	GameID                         string `json:"gameId"`
	Kind                           string `json:"kind"`
	PlayerID                       string `json:"playerId"`
	ProviderID                     string `json:"providerId"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId"`
	RoundID                        string `json:"roundId"`
	WalletID                       string `json:"walletId"`
}

// CanonicalPayload devolve o JSON canônico dos campos de negócio: mesma entrada,
// mesmos bytes — independentemente de ter chegado por HTTP ou por SQS.
func CanonicalPayload(f CanonicalPayloadFields) ([]byte, error) {
	if !f.Kind.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKind, f.Kind)
	}
	if f.Amount.Currency().IsZero() {
		return nil, fmt.Errorf("%w: amount has no currency", money.ErrUninitialized)
	}
	if isBlank(string(f.ProviderID)) {
		return nil, fmt.Errorf("%w: providerId", ErrMissingExternalMetadata)
	}
	if isBlank(string(f.ExternalTransactionID)) {
		return nil, fmt.Errorf("%w: externalTransactionId", ErrMissingExternalMetadata)
	}
	if isBlank(string(f.WalletID)) {
		return nil, fmt.Errorf("%w: walletId", ErrInvalidID)
	}

	// A forma canônica do valor é responsabilidade do Money: consumimos String().
	payload := canonicalPayload{
		Amount:                         f.Amount.String(),
		Currency:                       f.Amount.Currency().String(),
		ExternalTransactionID:          f.ExternalTransactionID.String(),
		GameID:                         f.GameID,
		Kind:                           f.Kind.String(),
		PlayerID:                       string(f.PlayerID),
		ProviderID:                     f.ProviderID.String(),
		ReferenceExternalTransactionID: f.ReferenceExternalTransactionID.String(),
		RoundID:                        f.RoundID,
		WalletID:                       string(f.WalletID),
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		// Só por bug de tipo; ainda assim erro em vez de panic.
		return nil, fmt.Errorf("transaction: cannot encode canonical payload: %w", err)
	}
	return encoded, nil
}

// ComputePayloadHash calcula o SHA-256 do payload canônico em hexadecimal minúsculo.
// Não é segurança: é identidade determinística do conteúdo (§9).
func ComputePayloadHash(f CanonicalPayloadFields) (PayloadHash, error) {
	payload, err := CanonicalPayload(f)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload) // [32]byte por valor, sem alocação no heap
	return NewPayloadHash(hex.EncodeToString(sum[:]))
}
