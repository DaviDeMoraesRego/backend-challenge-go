// Verificações de disponibilidade do /health/ready (§12). As dependências são abstratas
// por interface mínima (pinger, SQSAttributeClient): este pacote não importa o driver do
// banco e o teste de health check dispensa banco.
package observability

import (
	"context"
	"fmt"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// pinger é o mínimo que a checagem do banco precisa (pgxpool.Pool o satisfaz).
type pinger interface {
	Ping(ctx context.Context) error
}

type PostgresHealth struct {
	pool pinger
}

var _ interface {
	Name() string
	Check(ctx context.Context) error
} = (*PostgresHealth)(nil)

func NewPostgresHealth(pool pinger) *PostgresHealth {
	return &PostgresHealth{pool: pool}
}

func (h *PostgresHealth) Name() string { return "postgres" }

// Check faz um Ping com o contexto recebido (que já carrega timeout).
func (h *PostgresHealth) Check(ctx context.Context) error {
	if err := h.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// SQSAttributeClient é o mínimo que a checagem do broker precisa. Exportada: o
// composition root precisa citá-la ao compor o cliente único.
type SQSAttributeClient interface {
	GetQueueAttributes(ctx context.Context, params *awssqs.GetQueueAttributesInput, optFns ...func(*awssqs.Options)) (*awssqs.GetQueueAttributesOutput, error)
}

type SQSHealth struct {
	client   SQSAttributeClient
	queueURL string
}

var _ interface {
	Name() string
	Check(ctx context.Context) error
} = (*SQSHealth)(nil)

func NewSQSHealth(client SQSAttributeClient, queueURL string) *SQSHealth {
	return &SQSHealth{client: client, queueURL: queueURL}
}

func (h *SQSHealth) Name() string { return "sqs" }

// Check consulta um atributo simples da fila. GetQueueAttributes em vez de
// ReceiveMessage de propósito: receber mensagem mexeria na visibilidade de trabalho em
// andamento — health check não pode ter efeito colateral sobre o fluxo financeiro.
func (h *SQSHealth) Check(ctx context.Context) error {
	_, err := h.client.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl:       &h.queueURL,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		return fmt.Errorf("get queue attributes: %w", err)
	}
	return nil
}
