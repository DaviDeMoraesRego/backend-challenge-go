#!/usr/bin/env bash
# =============================================================================
# deploy/localstack/init/ready.d/01-create-queues.sh
# =============================================================================
# PROPÓSITO
#   Provisionar, de forma automática e idempotente, a topologia de filas SQS
#   exigida pelo §10 do spec:
#     - wager-transactions.fifo      (entrada de operações)
#     - wager-transactions-dlq.fifo  (DLQ da fila de entrada, com redrive)
#     - wager-events.fifo            (destino dos eventos da outbox, §11)
#     - wager-events-dlq.fifo        (DLQ dos eventos de saída)
#
# CAMADA DA ARQUITETURA HEXAGONAL
#   infrastructure bootstrap. Este script não é código de domínio nem adapter Go:
#   ele apenas materializa o broker que os adapters outbound (SQS publisher) e
#   inbound (SQS consumer) vão usar.
#
# POR QUE ASSIM
#   1. `ready.d` é o hook do LocalStack executado UMA vez, quando o serviço está
#      pronto para receber chamadas à API. Usar `boot.d` causaria
#      "connection refused" porque a API SQS ainda não estaria no ar.
#   2. `awslocal` é um wrapper do AWS CLI já instalado na imagem do LocalStack:
#      injeta endpoint/credenciais de teste automaticamente.
#   3. FIFO em todas as filas: o §8 exige coordenação POR CARTEIRA, e o
#      `MessageGroupId` (que só existe em fila FIFO) é o mecanismo de ordenação
#      por grupo. A serialização física real continua no banco (§5.3).
#   4. `ContentBasedDeduplication=false`: enviamos o `MessageDeduplicationId`
#      explicitamente (o eventId estável, §11), preservando-o em republicações.
#   5. A DLQ é criada PRIMEIRO porque a RedrivePolicy precisa do ARN dela.
#      `maxReceiveCount=5` move para a DLQ após a quinta tentativa (§10).
#
# ARMADILHA QUE CUSTOU UMA CORREÇÃO (registrada aqui de propósito)
#   A RedrivePolicy NÃO pode ser passada pelo parser "shorthand" do
#   `--attributes chave=valor,chave=valor`: a política é um JSON que contém
#   RÓTULOS, VÍRGULAS e ASPAS, e o parser do AWS CLI tenta dividir o valor nas
#   vírgulas e encontra aspas onde esperava '='. O erro é:
#
#       Error parsing parameter '--attributes': Expected: '=', received: '"'
#
#   A solução é enviar o documento JSON inteiro via `--cli-input-json file://...`,
#   que não passa pelo shorthand. É o padrão correto para parâmetros aninhados.
#
# VALIDAÇÃO LOCAL
#   awslocal sqs list-queues
#   awslocal sqs get-queue-attributes \
#       --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
#       --attribute-names All
#
# ARMADILHA CLÁSSICA DE SHELL
#   `set -euo pipefail` faz o script morrer no primeiro erro. Sem isso, a falha
#   acima ficaria silenciosa e só descobriríamos em runtime, com a fila principal
#   inexistente — um bug caro.
# =============================================================================

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
