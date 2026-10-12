// UUIDv7 (e não v4): o v7 começa com timestamp em ordem crescente, então IDs gerados
// em sequência ficam próximos no índice B-Tree — a localidade de um BIGSERIAL sem abrir
// mão de gerar o ID no cliente (conhecido antes do INSERT e estável entre tentativas).
// Troca registrada: revela o instante aproximado de criação (aceitável, útil em auditoria).
package idgen

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

type UUIDv7 struct{}

var _ ports.IDGenerator = (*UUIDv7)(nil)

func New() *UUIDv7 { return &UUIDv7{} }

// NewID gera um UUIDv7 em formato canônico (36 caracteres). O `ctx` cumpre a porta e
// deixa a assinatura pronta para implementações futuras que respeitem cancelamento.
func (g *UUIDv7) NewID(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		// Respeita o cancelamento antes de gerar um ID para uma requisição já desistida.
		return "", err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("idgen: cannot generate uuidv7: %w", err)
	}
	return id.String(), nil
}
