package transaction

// FailureCode é o código estável de rejeição/falha de uma transação.
type FailureCode string

const (
	// FailureBetInsufficientFunds: aposta recusada por saldo insuficiente.
	FailureBetInsufficientFunds FailureCode = "BET_INSUFFICIENT_FUNDS"

	// FailureReversalInsufficientFunds: reversão recusada porque o débito excederia
	// o saldo (§7 usa código diferente de uma aposta sem saldo).
	FailureReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"

	// FailureReferenceNotFound: referência não apareceu dentro do TTL/tentativas
	// máximas (§7).
	FailureReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"

	// FailureReferenceNotProcessed: referência existe mas terminou sem sucesso.
	FailureReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"

	// FailureReferenceStillPending: referência existe, porém ainda pendente
	// (transitório: mantém em PENDING_REFERENCE).
	FailureReferenceStillPending FailureCode = "REFERENCE_STILL_PENDING"

	// FailureDuplicateReversal: a referência já possui reversão bem-sucedida.
	FailureDuplicateReversal FailureCode = "DUPLICATE_REVERSAL"

	// FailureReferenceMismatch: referência discorda em provedor, jogador, carteira,
	// moeda ou rodada (§7).
	FailureReferenceMismatch FailureCode = "REFERENCE_MISMATCH"

	// FailureReferenceKindNotAllowed: tipo referenciado não pode ser revertido por
	// este tipo de reversão (ex.: ROLLBACK de um LOSS).
	FailureReferenceKindNotAllowed FailureCode = "REFERENCE_KIND_NOT_ALLOWED"

	// FailureAmountMismatch: valor da reversão difere do referenciado (§7).
	FailureAmountMismatch FailureCode = "AMOUNT_MISMATCH"

	// FailureInvalidAmount: valor ausente, malformado ou em desacordo com o tipo.
	FailureInvalidAmount FailureCode = "INVALID_AMOUNT"

	// FailureInvalidKind: tipo desconhecido ou OPENING por canal externo.
	FailureInvalidKind FailureCode = "INVALID_KIND"

	// FailureCurrencyMismatch: moeda da operação difere da moeda da carteira.
	FailureCurrencyMismatch FailureCode = "CURRENCY_MISMATCH"

	// FailureWalletNotFound: carteira inexistente para o jogador/moeda informado.
	FailureWalletNotFound FailureCode = "WALLET_NOT_FOUND"

	// FailureIdempotencyConflict: mesma chave com conteúdo diferente, ou
	// (providerId, externalTransactionId) reutilizado com outra chave (§9).
	FailureIdempotencyConflict FailureCode = "IDEMPOTENCY_CONFLICT"

	// FailureInternal: falha permanente de infraestrutura (§6.3), não é erro de
	// negócio e não deve ser exposta como tal ao provedor.
	FailureInternal FailureCode = "INTERNAL_ERROR"
)

// validFailureCodes é a lista fechada de códigos aceitos (valida construtores e
// documenta o contrato).
var validFailureCodes = map[FailureCode]struct{}{
	FailureBetInsufficientFunds:      {},
	FailureReversalInsufficientFunds: {},
	FailureReferenceNotFound:         {},
	FailureReferenceNotProcessed:     {},
	FailureReferenceStillPending:     {},
	FailureDuplicateReversal:         {},
	FailureReferenceMismatch:         {},
	FailureReferenceKindNotAllowed:   {},
	FailureAmountMismatch:            {},
	FailureInvalidAmount:             {},
	FailureInvalidKind:               {},
	FailureCurrencyMismatch:          {},
	FailureWalletNotFound:            {},
	FailureIdempotencyConflict:       {},
	FailureInternal:                  {},
}

func (c FailureCode) IsValid() bool {
	_, ok := validFailureCodes[c]
	return ok
}

func (c FailureCode) String() string { return string(c) }

// IsCorrectable: falha CORRIGÍVEL (o provedor resolve reenviando entrada ajustada
// ou esperando a condição mudar) vs. DEFINITIVA (§7).
//
// Corrigíveis: entradas malformadas (INVALID_*), divergência de moeda/valor/
// referência e "referência ainda pendente". Definitivas: saldo insuficiente,
// duplicidade de reversão, tipo não reversível, carteira inexistente, conflito de
// idempotência e falha interna.
//
// BET_INSUFFICIENT_FUNDS é definitiva para ESTA operação: só uma operação nova
// (outra rodada/valor) teria sucesso.
func (c FailureCode) IsCorrectable() bool {
	switch c {
	case FailureInvalidAmount,
		FailureInvalidKind,
		FailureCurrencyMismatch,
		FailureAmountMismatch,
		FailureReferenceMismatch,
		FailureReferenceNotFound,
		FailureReferenceStillPending:
		return true
	default:
		return false
	}
}
