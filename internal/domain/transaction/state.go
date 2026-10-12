package transaction

import (
	"fmt"
	"slices"
)

// ErrInvalidTransition: transição não permitida (inclui qualquer saída de estado terminal).
var ErrInvalidTransition = fmt.Errorf("transaction: invalid state transition")

// ErrInvalidState: estado desconhecido (ex.: valor corrompido lido do banco).
var ErrInvalidState = fmt.Errorf("transaction: invalid state")

// State é o estado da `WagerTransaction` (§6.3).
type State string

const (
	// StatePending: registro aceito, processamento ainda não concluído.
	StatePending State = "PENDING"

	// StatePendingReference: depende de uma referência ainda indisponível (§3/§7).
	StatePendingReference State = "PENDING_REFERENCE"

	// StateProcessed: concluída com sucesso; terminal.
	StateProcessed State = "PROCESSED"

	// StateRejected: recusada por regra de negócio; terminal.
	StateRejected State = "REJECTED"

	// StateFailed: falha permanente de infraestrutura; terminal.
	StateFailed State = "FAILED"
)

var validStates = map[State]struct{}{
	StatePending:          {},
	StatePendingReference: {},
	StateProcessed:        {},
	StateRejected:         {},
	StateFailed:           {},
}

// allowedTransitions é a tabela da máquina de estados: "de X posso ir para a lista".
// Estados terminais não aparecem como chave — logo, não têm saída.
//
// A auto-transição em PENDING_REFERENCE é intencional: cada tentativa frustrada do
// worker rearma a espera, incrementando `attempts` e empurrando `nextRetryAt` (§7).
var allowedTransitions = map[State][]State{
	StatePending: {
		StatePendingReference,
		StateProcessed,
		StateRejected,
		StateFailed,
	},
	StatePendingReference: {
		StatePendingReference, // rearmar a espera (não incrementa estado terminal)
		StateProcessed,
		StateRejected,
		StateFailed,
	},
}

// ParseState valida um texto vindo do storage (protege contra dado corrompido).
func ParseState(raw string) (State, error) {
	state := State(raw)
	if !state.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrInvalidState, raw)
	}
	return state, nil
}

func (s State) IsValid() bool {
	_, ok := validStates[s]
	return ok
}

func (s State) String() string { return string(s) }

// IsTerminal: "sem saída na tabela" mantém a lista de terminais sincronizada com
// as transições.
func (s State) IsTerminal() bool {
	return len(allowedTransitions[s]) == 0
}

// CanTransitionTo: não há caso especial para "mesmo estado" — PROCESSED->PROCESSED
// cai em IsTerminal e é negado.
func (s State) CanTransitionTo(target State) bool {
	if !s.IsValid() || !target.IsValid() {
		return false
	}
	return slices.Contains(allowedTransitions[s], target)
}

// validateTransition: guarda interno das transições do agregado.
func (s State) validateTransition(target State) error {
	if !s.CanTransitionTo(target) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, s, target)
	}
	return nil
}
