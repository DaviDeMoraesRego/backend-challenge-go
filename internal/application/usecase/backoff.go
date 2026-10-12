package usecase

import "time"

// maxBackoffShift limita o expoente: sem ele, `1 << attempts` estoura o int64 a
// partir de 63 tentativas (e antes disso já dá atrasos absurdos). Shift <= 20 limita
// o fator a ~1.048.576.
const maxBackoffShift = 20

// backoffCeiling limita o resultado final (mais fácil de auditar que um expoente no olho).
const backoffCeiling = time.Hour

// exponentialBackoff devolve o atraso da tentativa N (1-based): base, base*2,
// base*4... até o teto de 1 hora. Recebe `attempts` já incrementado, o que casa com
// Transaction.MarkPendingReference.
func exponentialBackoff(base time.Duration, attempts int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if attempts <= 1 {
		return base
	}

	shift := min(attempts-1, maxBackoffShift)

	delay := base * time.Duration(1<<shift)
	if delay <= 0 || delay > backoffCeiling {
		// `delay <= 0` cobre overflow (virou negativo); nesse caso, teto.
		return backoffCeiling
	}
	return delay
}
