// Implementação real da porta Clock (o fake vive nos testes). Sempre UTC: o §11 exige
// RFC 3339 em UTC, e normalizar aqui — na única porta de leitura do tempo — impede que
// qualquer ponto do sistema introduza fuso por descuido.
package clock

import (
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

type System struct{}

var _ ports.Clock = (*System)(nil)

func New() *System { return &System{} }

// Now devolve o instante atual em UTC.
func (c *System) Now() time.Time { return time.Now().UTC() }
