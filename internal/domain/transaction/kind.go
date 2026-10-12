package transaction

import "fmt"

var ErrInvalidKind = fmt.Errorf("transaction: invalid kind")

// Kind é o tipo da operação de aposta.
type Kind string

// Vocabulário completo (§6.3). Os textos casam exatamente com o CHECK da coluna
// `kind` em `wager_transactions`.
const (
	// KindOpening é reservado à abertura interna; rejeitado por HTTP/SQS (§6.3).
	KindOpening Kind = "OPENING"

	// KindBet debita a carteira; exige valor positivo e saldo suficiente (§7).
	KindBet Kind = "BET"

	// KindWin credita a carteira; pode referenciar uma aposta da mesma rodada (§7).
	KindWin Kind = "WIN"

	// KindLoss não movimenta a carteira (valor "0.00"), mas produz
	// WagerTransactionProcessed (§7/§11).
	KindLoss Kind = "LOSS"

	// KindRefund devolve integralmente o valor de uma BET processada (§7).
	KindRefund Kind = "REFUND"

	// KindRollback desfaz integralmente uma BET, WIN ou REFUND processada (§7).
	KindRollback Kind = "ROLLBACK"
)

// validKinds usa map[Kind]struct{} (set idiomático): só a presença da chave importa.
var validKinds = map[Kind]struct{}{
	KindOpening:  {},
	KindBet:      {},
	KindWin:      {},
	KindLoss:     {},
	KindRefund:   {},
	KindRollback: {},
}

// ParseKind converte texto de fronteira em Kind validado. Não normaliza a caixa —
// aceitar "bet" mudaria o hash de idempotência de forma invisível.
func ParseKind(raw string) (Kind, error) {
	kind := Kind(raw)
	if !kind.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidKind, raw)
	}
	return kind, nil
}

func (k Kind) IsValid() bool {
	_, ok := validKinds[k]
	return ok
}

func (k Kind) String() string { return string(k) }

// IsInternal: somente OPENING nasce da abertura de carteira, nunca de um provedor.
func (k Kind) IsInternal() bool { return k == KindOpening }

func (k Kind) IsExternal() bool { return k.IsValid() && k != KindOpening }

// IsReversal: REFUND/ROLLBACK exigem referência.
func (k Kind) IsReversal() bool { return k == KindRefund || k == KindRollback }

// AllowsZeroAmount: só LOSS (§7) e OPENING (§9) aceitam zero; os demais exigem > 0.
func (k Kind) AllowsZeroAmount() bool {
	switch k {
	case KindLoss, KindOpening:
		return true
	default:
		return false
	}
}

func (k Kind) RequiresPositiveAmount() bool { return !k.AllowsZeroAmount() }

// MovesWallet: LOSS é o único tipo externo que não movimenta (§7) — por isso o
// caso de uso não chama Debit/Credit em LOSS.
func (k Kind) MovesWallet() bool { return k != KindLoss }

// AllowsReferenceFrom: REFUND devolve uma BET; ROLLBACK desfaz BET, WIN ou REFUND
// (§7). O que não está na lista é proibido por construção.
func (k Kind) AllowsReferenceFrom(reversal Kind) bool {
	switch reversal {
	case KindRefund:
		return k == KindBet
	case KindRollback:
		return k == KindBet || k == KindWin || k == KindRefund
	default:
		return false
	}
}

// Origin distingue operações internas das que chegam de provedores externos.
// O schema impõe a distinção (§6.3): provider_id e idempotency_key são obrigatórios
// para EXTERNAL e proibidos para INTERNAL.
type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

func (o Origin) IsValid() bool { return o == OriginInternal || o == OriginExternal }

func (o Origin) String() string { return string(o) }

// OriginForKind deriva a origem do tipo: OPENING é sempre interno; o resto é externo.
func OriginForKind(kind Kind) (Origin, error) {
	if !kind.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidKind, kind)
	}
	if kind.IsInternal() {
		return OriginInternal, nil
	}
	return OriginExternal, nil
}
