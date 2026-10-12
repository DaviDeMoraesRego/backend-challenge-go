// Política de destino da mensagem (§10):
//   - processou (PROCESSED/REJECTED/PENDING_REFERENCE) -> DeleteMessage (rejeição é
//     terminal; reentrega daria o mesmo resultado);
//   - erro TRANSITÓRIO (banco/broker fora) -> NÃO deletar (volta após o visibility
//     timeout, at-least-once, §3);
//   - mensagem inválida / erro permanente -> NÃO deletar (o maxReceiveCount leva à DLQ).
//
// Evita deletar numa falha transitória (perde a operação) ou manter numa rejeição de
// negócio (laço infinito).
package sqsworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
)

// Client é o mínimo que o consumidor precisa do SDK (declarada pelo consumidor,
// testável com fake). Exportada: o composition root compõe o cliente único que serve
// publisher, consumer e health check.
type Client interface {
	ReceiveMessage(ctx context.Context, params *awssqs.ReceiveMessageInput, optFns ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *awssqs.DeleteMessageInput, optFns ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error)
}

// Config reúne os parâmetros de polling.
type Config struct {
	QueueURL          string
	ConsumerName      string
	MaxMessages       int32
	WaitTimeSeconds   int32
	VisibilityTimeout int32
}

// Consumer lê a fila e executa o caso de uso de operações.
type Consumer struct {
	client Client
	place  ports.PlaceWagerOperation
	cfg    Config
	logger *slog.Logger
}

func NewConsumer(client Client, place ports.PlaceWagerOperation, cfg Config, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{client: client, place: place, cfg: cfg, logger: logger}
}

// Run mantém o laço de polling até o contexto ser cancelado. Retorna `nil` no
// cancelamento: encerrar por SIGTERM é o caminho esperado, não uma falha.
func (c *Consumer) Run(ctx context.Context) error {
	c.logger.Info("sqs consumer iniciado",
		slog.String("queue_url", c.cfg.QueueURL),
		slog.String("consumer", c.cfg.ConsumerName),
	)

	for {
		// Checagem no topo do laço: durante o shutdown, paramos de BUSCAR trabalho
		// novo imediatamente, sem esperar uma rodada inteira (§4).
		if err := ctx.Err(); err != nil {
			c.logger.Info("sqs consumer encerrado")
			return nil
		}

		output, err := c.client.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:            &c.cfg.QueueURL,
			MaxNumberOfMessages: c.cfg.MaxMessages,
			// Long polling: o broker segura a conexão até haver mensagem.
			WaitTimeSeconds: c.cfg.WaitTimeSeconds,
			// Prazo para concluirmos o commit antes de liberar a mensagem.
			VisibilityTimeout: c.cfg.VisibilityTimeout,
			// No pedido usamos QueueAttributeName; o mapa de message.Attributes é
			// indexado por MessageSystemAttributeName (tipos distintos no SDK v2).
			AttributeNames:        []types.QueueAttributeName{types.QueueAttributeNameAll},
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Erro TRANSITÓRIO de leitura: loga e recua antes de tentar de novo.
			// Repetir imediatamente num broker fora só piora a situação.
			c.logger.Error("falha ao ler da fila", slog.String("error", err.Error()))
			if !sleepWithContext(ctx, 2*time.Second) {
				return nil
			}
			continue
		}

		// Processamento sequencial dentro do lote: previsível e sem esgotar o pool de
		// conexões de uma instância (o paralelismo vem das instâncias).
		for _, message := range output.Messages {
			c.processMessage(ctx, message)
		}
	}
}

// processMessage trata UMA mensagem e decide seu destino.
func (c *Consumer) processMessage(ctx context.Context, message types.Message) {
	// Prazo por mensagem alinhado ao visibility timeout: uma mensagem travada não
	// segura o worker (além do prazo ela já seria reentregue).
	if c.cfg.VisibilityTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(c.cfg.VisibilityTimeout)*time.Second)
		defer cancel()
	}

	if message.Body == nil {
		c.logger.Error("mensagem sem corpo; deixando para a DLQ")
		return
	}
	body := *message.Body
	receiveCount := receiveCount(message)

	envelope, err := parseEnvelope(body)
	if err != nil {
		// MENSAGEM INVÁLIDA: nenhum retry a torna válida. Não deletamos — o
		// maxReceiveCount do redrive a leva à DLQ (§10), preservando a evidência.
		c.logger.Error("mensagem invalida; sera enviada a DLQ apos esgotar tentativas",
			slog.String("sqs_message_id", sqsMessageID(message)),
			slog.Int("receive_count", receiveCount),
			slog.String("error", err.Error()),
		)
		return
	}

	// Identidade INTERNA: quem acessa o broker é o próprio sistema (§2). O providerId
	// vem do corpo e as regras de negócio seguem aplicadas no consumidor — o broker
	// controla acesso, não negócio.
	command := ports.PlaceOperationCommand{
		Principal: ports.Principal{Scope: ports.ScopeInternal},

		ProviderID:                     envelope.Data.ProviderID,
		ExternalTransactionID:          envelope.Data.ExternalTransactionID,
		IdempotencyKey:                 envelope.Data.IdempotencyKey,
		PlayerID:                       envelope.Data.PlayerID,
		WalletID:                       envelope.Data.WalletID,
		RoundID:                        envelope.Data.RoundID,
		GameID:                         envelope.Data.GameID,
		Kind:                           envelope.Data.Kind,
		Amount:                         envelope.Data.Money.Amount,
		Currency:                       envelope.Data.Money.Currency,
		ReferenceExternalTransactionID: envelope.Data.ReferenceExternalTransactionID,

		CorrelationID: envelope.MessageID,

		// Inbox (§6.5/§10): a identidade durável da mensagem é o `messageId` do
		// ENVELOPE, e o hash do corpo detecta reentrega com conteúdo diferente.
		InboxConsumer:    c.cfg.ConsumerName,
		InboxMessageID:   envelope.MessageID,
		InboxMessageHash: hashOf(body),
	}

	result, err := c.place.Execute(ctx, command)
	switch {
	case err == nil:
		// Inclui PROCESSED, REJECTED e PENDING_REFERENCE: os três são conclusões
		// duráveis (rejeição é terminal §10; a pendência já foi persistida §6.5).
		c.logger.Info("mensagem processada",
			slog.String("transaction_id", result.TransactionID.String()),
			slog.String("state", result.State.String()),
			slog.String("failure_code", result.FailureCode.String()),
			slog.Bool("idempotent_replay", result.IdempotentReplay),
			slog.String("correlation_id", envelope.MessageID),
		)
		c.deleteMessage(ctx, message, envelope.MessageID)

	case errors.Is(err, ports.ErrUnavailable):
		// TRANSITÓRIO: NÃO deletar. A mensagem reaparece após o visibility timeout e
		// será retentada — possivelmente por outra instância.
		c.logger.Warn("falha transitoria; mensagem sera reentregue",
			slog.String("sqs_message_id", sqsMessageID(message)),
			slog.Int("receive_count", receiveCount),
			slog.String("error", err.Error()),
		)

	default:
		// PERMANENTE (validação, conflito de idempotência, mensagem inválida):
		// retry não muda o resultado. Deixamos a mensagem seguir para a DLQ.
		c.logger.Error("erro permanente; mensagem sera enviada a DLQ",
			slog.String("sqs_message_id", sqsMessageID(message)),
			slog.Int("receive_count", receiveCount),
			slog.String("error", err.Error()),
		)
	}
}

// deleteMessage remove a mensagem da fila DEPOIS do commit (§10).
func (c *Consumer) deleteMessage(ctx context.Context, message types.Message, correlationID string) {
	receiptHandle := message.ReceiptHandle
	if receiptHandle == nil {
		c.logger.Error("mensagem sem receipt handle; nao foi possivel remover")
		return
	}

	// Best-effort: se falhar, a mensagem é reentregue e a idempotência transforma o
	// reprocessamento em replay — sem efeito duplicado.
	if _, err := c.client.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl:      &c.cfg.QueueURL,
		ReceiptHandle: receiptHandle,
	}); err != nil {
		c.logger.Warn("falha ao remover mensagem da fila",
			slog.String("correlation_id", correlationID),
			slog.String("error", err.Error()),
		)
	}
}

// messageEnvelope espelha o contrato do §10.
type messageEnvelope struct {
	MessageID  string      `json:"messageId"`
	Type       string      `json:"type"`
	OccurredAt time.Time   `json:"occurredAt"`
	Data       messageData `json:"data"`
}

// messageData é o corpo de negócio da mensagem.
type messageData struct {
	ProviderID                     string    `json:"providerId"`
	ExternalTransactionID          string    `json:"externalTransactionId"`
	IdempotencyKey                 string    `json:"idempotencyKey"`
	PlayerID                       string    `json:"playerId"`
	WalletID                       string    `json:"walletId"`
	RoundID                        string    `json:"roundId"`
	GameID                         string    `json:"gameId"`
	Kind                           string    `json:"kind"`
	Money                          moneyJSON `json:"money"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId"`
}

// moneyJSON recebe valor e moeda como STRINGS (§5.1: dinheiro nunca em número).
type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// parseEnvelope valida o essencial (estrutural); as regras de negócio ficam no caso de uso.
func parseEnvelope(body string) (messageEnvelope, error) {
	var envelope messageEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return messageEnvelope{}, fmt.Errorf("envelope invalido: %w", err)
	}

	if envelope.MessageID == "" {
		return messageEnvelope{}, errors.New("envelope sem messageId (identidade duravel exigida pelo §10)")
	}
	if envelope.Data.IdempotencyKey == "" {
		return messageEnvelope{}, errors.New("data.idempotencyKey ausente (chave da idempotencia no SQS, §10)")
	}

	return envelope, nil
}

// hashOf = SHA-256 do corpo (hex). Detecta o cenário do §10: mesmo messageId com
// conteúdo DIFERENTE.
func hashOf(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// receiveCount extrai ApproximateReceiveCount (entregas já feitas) — útil para DLQ.
func receiveCount(message types.Message) int {
	raw, ok := message.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)]
	if !ok {
		return 0
	}
	var count int
	if _, err := fmt.Sscanf(raw, "%d", &count); err != nil {
		return 0
	}
	return count
}

// sqsMessageID devolve o `MessageId` do BROKER (não o do envelope), útil para
// correlacionar com os logs do próprio SQS.
func sqsMessageID(message types.Message) string {
	if message.MessageId == nil {
		return ""
	}
	return *message.MessageId
}

// sleepWithContext espera `d` OU até o contexto ser cancelado. `time.Sleep` não é
// cancelável: durante o shutdown o worker ficaria preso até o fim da espera.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
