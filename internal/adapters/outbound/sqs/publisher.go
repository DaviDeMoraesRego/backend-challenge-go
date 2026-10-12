// Filas FIFO: MessageGroupId = aggregateId (ordem por agregado — carteira nos eventos
// de saldo, transação nos de operação; agregados diferentes podem intercalar).
// MessageDeduplicationId = eventId estável entre republicações (§11): republicar não
// cria mensagem nova na janela de 5 min da SQS e, depois dela, o consumidor deduplica.
package sqs

import (
	"context"
	"fmt"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

// PublisherClient é o mínimo que o publisher precisa do SDK (permite testar sem
// LocalStack). Exportada: o composition root cita este tipo para declarar o cliente
// único que atende publisher, consumer e health check.
type PublisherClient interface {
	SendMessage(ctx context.Context, params *awssqs.SendMessageInput, optFns ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error)
}

// Publisher publica registros de outbox numa fila FIFO.
type Publisher struct {
	client   PublisherClient
	queueURL string
}

var _ ports.EventPublisher = (*Publisher)(nil)

func NewPublisher(client PublisherClient, queueURL string) *Publisher {
	return &Publisher{client: client, queueURL: queueURL}
}

// Publish envia o evento.
func (p *Publisher) Publish(ctx context.Context, record ports.OutboxRecord, groupID string) error {
	if groupID == "" {
		// Sem grupo, uma fila FIFO recusa a mensagem. Falhar aqui, com mensagem
		// clara, é melhor que receber um erro opaco da AWS.
		return fmt.Errorf("sqs: event %s has no MessageGroupId", record.EventID)
	}

	input := &awssqs.SendMessageInput{
		QueueUrl:               &p.queueURL,
		MessageBody:            strPtr(string(record.Payload)),
		MessageGroupId:         &groupID,
		MessageDeduplicationId: strPtr(record.EventID),
		MessageAttributes:      messageAttributes(record),
	}

	if _, err := p.client.SendMessage(ctx, input); err != nil {
		return fmt.Errorf("%w: sending event %s: %v", ports.ErrUnavailable, record.EventID, err)
	}
	return nil
}

// messageAttributes leva a correlação ao lado do corpo: atributos são visíveis para
// roteamento do broker e diagnóstico, sem desserializar o payload.
func messageAttributes(record ports.OutboxRecord) map[string]types.MessageAttributeValue {
	attributes := map[string]types.MessageAttributeValue{
		"eventType": {DataType: strPtr("String"), StringValue: strPtr(record.EventType)},
	}
	if record.CorrelationID != "" {
		attributes["correlationId"] = types.MessageAttributeValue{
			DataType: strPtr("String"), StringValue: strPtr(record.CorrelationID),
		}
	}
	if record.CausationID != "" {
		attributes["causationId"] = types.MessageAttributeValue{
			DataType: strPtr("String"), StringValue: strPtr(record.CausationID),
		}
	}
	return attributes
}

// strPtr: o SDK usa *string nos campos opcionais para distinguir "não enviado" de
// "enviado vazio"; não há helper genérico no SDK v2.
func strPtr(s string) *string { return &s }
