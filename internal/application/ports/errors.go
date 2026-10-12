package ports

import "errors"

// Taxonomia de erros da fronteira de aplicação, com o mapeamento para status HTTP.
// Rejeição de negócio NÃO é erro: ela volta em PlaceOperationResult com State=REJECTED.
var (
	// ErrValidation -> 400/422. O mesmo motivo dentro do fluxo de operação (ex.:
	// valor zero numa BET) pode virar REJECTED com INVALID_AMOUNT (§9).
	ErrValidation = errors.New("ports: validation failed")

	// ErrForbidden -> 403: identidade sem permissão (§2), sem efeito financeiro nem
	// exposição de dados de outro provedor (§13).
	ErrForbidden = errors.New("ports: forbidden")

	// ErrNotFound -> 404: os adapters devolvem quando a consulta não encontra linha.
	ErrNotFound = errors.New("ports: not found")

	// ErrAlreadyExists: violação de unicidade. Chave de idempotência já usada -> REPLAY;
	// (providerId, externalTransactionId) com outra chave -> 409; (playerId, currency)
	// duplicado na abertura (§9).
	ErrAlreadyExists = errors.New("ports: already exists")

	// ErrConflict: conflito que não é de unicidade (ex.: lock otimista). A estratégia
	// principal é pessimista (§8); serve de destino para deadlock/serialization failure.
	ErrConflict = errors.New("ports: conflict")

	// ErrUnavailable: banco ou broker indisponível. NUNCA vira REJECTED — significa
	// "tente de novo": HTTP 503, e no SQS a mensagem volta após o visibility timeout (§10).
	ErrUnavailable = errors.New("ports: dependency unavailable")
)
