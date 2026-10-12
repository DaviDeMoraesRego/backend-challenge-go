#!/usr/bin/env bash
# Provisiona (idempotente) a topologia de filas SQS FIFO do §10/§11:
#   wager-transactions.fifo / -dlq.fifo   entrada de operações (redrive maxReceiveCount=5)
#   wager-events.fifo       / -dlq.fifo   eventos da outbox
#
# `ready.d` roda UMA vez, quando o LocalStack está pronto (com `boot.d` haveria
# "connection refused"). FIFO em todas: o MessageGroupId ordena por grupo; a
# serialização física real é o lock de linha no banco (§5.3).
#
# ARMADILHA: a RedrivePolicy é um JSON com vírgulas e aspas e NÃO passa pelo parser
# shorthand do `--attributes` ("Expected: '=', received: '\"'"); usar
# `--cli-input-json`. `set -euo pipefail` faz o script morrer no primeiro erro.

set -euo pipefail

# Região consistente com docker-compose e com o SDK Go: produtor e consumidor
# precisam assinar com a MESMA região, senão a assinatura SigV4 é rejeitada.
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"

# Endpoint base do LocalStack. Usamos a forma "localhost" nas URLs internas para
# que o mesmo script funcione executado de dentro do container ou do host.
LOCALSTACK_URL="http://localhost:4566"
ACCOUNT_ID="000000000000"

echo "[localstack-init] criando topologia de filas SQS FIFO..."

# -----------------------------------------------------------------------------
# 1) DLQ da fila de entrada (criada primeiro: a RedrivePolicy precisa do ARN).
#    Atributos simples, sem vírgulas nos valores, cabem no shorthand do CLI.
# -----------------------------------------------------------------------------
awslocal sqs create-queue \
  --queue-name wager-transactions-dlq.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false

# -----------------------------------------------------------------------------
# 2) Fila principal de entrada, com redrive apontando para a DLQ.
#    VisibilityTimeout=30s: o worker tem 30 segundos para concluir (commit no
#    banco) antes de a mensagem reaparecer para outra instância — é o
#    mecanismo que sustenta o "at-least-once" do §3.
# -----------------------------------------------------------------------------
TRANSACTIONS_DLQ_ARN="$(awslocal sqs get-queue-attributes \
  --queue-url "${LOCALSTACK_URL}/${ACCOUNT_ID}/wager-transactions-dlq.fifo" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' \
  --output text)"

# Arquivo temporário com o input JSON completo. `mktemp` cria com permissão
# restritiva; `trap ... EXIT` garante a limpeza mesmo se o script falhar no meio.
QUEUE_INPUT_FILE="$(mktemp)"
trap 'rm -f "${QUEUE_INPUT_FILE}"' EXIT

cat > "${QUEUE_INPUT_FILE}" <<JSON
{
  "QueueName": "wager-transactions.fifo",
  "Attributes": {
    "FifoQueue": "true",
    "ContentBasedDeduplication": "false",
    "VisibilityTimeout": "30",
    "RedrivePolicy": "{\"deadLetterTargetArn\":\"${TRANSACTIONS_DLQ_ARN}\",\"maxReceiveCount\":\"5\"}"
  }
}
JSON

awslocal sqs create-queue --cli-input-json "file://${QUEUE_INPUT_FILE}"

# -----------------------------------------------------------------------------
# 3) Fila de eventos de saída (§11). O publisher da outbox publica aqui, sempre
#    DEPOIS do commit (nunca antes). MessageGroupId = aggregateId (walletId),
#    preservando a ordem por carteira.
#
#    DLQ criada primeiro; a política vai via --cli-input-json (ver a armadilha acima).
# -----------------------------------------------------------------------------
awslocal sqs create-queue \
  --queue-name wager-events-dlq.fifo \
  --attributes FifoQueue=true,ContentBasedDeduplication=false

EVENTS_DLQ_ARN="$(awslocal sqs get-queue-attributes \
  --queue-url "${LOCALSTACK_URL}/${ACCOUNT_ID}/wager-events-dlq.fifo" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' \
  --output text)"

EVENTS_INPUT_FILE="$(mktemp)"
trap 'rm -f "${QUEUE_INPUT_FILE}" "${EVENTS_INPUT_FILE}"' EXIT

cat > "${EVENTS_INPUT_FILE}" <<JSON
{
  "QueueName": "wager-events.fifo",
  "Attributes": {
    "FifoQueue": "true",
    "ContentBasedDeduplication": "false",
    "VisibilityTimeout": "30",
    "RedrivePolicy": "{\"deadLetterTargetArn\":\"${EVENTS_DLQ_ARN}\",\"maxReceiveCount\":\"5\"}"
  }
}
JSON

awslocal sqs create-queue --cli-input-json "file://${EVENTS_INPUT_FILE}"

echo "[localstack-init] filas criadas:"
awslocal sqs list-queues
