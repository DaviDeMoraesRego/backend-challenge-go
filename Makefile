# =============================================================================
# Makefile
# =============================================================================
# PROPÓSITO
#   Atalhos reproduzíveis para o fluxo de desenvolvimento: subir o ambiente,
#   aplicar/reverter migrations, rodar testes e inspecionar recursos. Atende ao
#   §15 (comandos reproduzíveis a partir de um checkout limpo).
#
# CAMADA DA ARQUITETURA HEXAGONAL
#   infrastructure bootstrap (ferramental de operação).
#
# POR QUE ASSIM
#   - Todos os comandos passam por `docker compose`, que é o único pré-requisito
#     além do Go. Nada depende de ferramentas instaladas globalmente (psql, aws
#     cli, migrate) — usamos os containers para isso.
#   - Alvos granulares permitem validar cada peça isoladamente durante o
#     desenvolvimento (ex.: `make migrate-up` sem subir a app).
#
# ATENÇÃO (Windows/cmd)
#   Arquivos Makefile usam TAB para indentar comandos; espaços causam o erro
#   "missing separator". Este arquivo já usa TAB. No Windows, rode via WSL,
#   Git Bash ou `make` do Chocolatey/Scoop.
#
# VALIDAÇÃO
#   make up        # sobe Postgres, LocalStack, Keycloak e aplica migrations
#   make ps        # mostra o status dos containers
#   make queues    # lista as filas provisionadas
# =============================================================================

# Comando base do Compose (uma vez definido, evita repetição e erro de digitação).
COMPOSE := docker compose

# Liga `make` como o alvo padrão explícito, mesmo se o primeiro alvo mudar.
.DEFAULT_GOAL := help

# -----------------------------------------------------------------------------
# Ambiente local
# -----------------------------------------------------------------------------
.PHONY: up
up: ## Sobe toda a infraestrutura local e aplica as migrations
	$(COMPOSE) up -d
	$(COMPOSE) run --rm migrate

.PHONY: down
down: ## Derruba os containers (mantém os volumes/dados)
	$(COMPOSE) down

.PHONY: clean
clean: ## Derruba containers E apaga volumes (reset total do ambiente)
	$(COMPOSE) down -v

.PHONY: ps
ps: ## Mostra o status dos containers
	$(COMPOSE) ps

.PHONY: logs
logs: ## Acompanha os logs de todos os serviços
	$(COMPOSE) logs -f

# -----------------------------------------------------------------------------
# Migrations (golang-migrate)
# -----------------------------------------------------------------------------
.PHONY: migrate-up
migrate-up: ## Aplica todas as migrations pendentes
	$(COMPOSE) run --rm migrate -path=/migrations \
		-database="postgres://wager:wager@postgres:5432/wager?sslmode=disable" up

.PHONY: migrate-down
migrate-down: ## Reverte TODAS as migrations (usa o .down.sql)
	$(COMPOSE) run --rm migrate -path=/migrations \
		-database="postgres://wager:wager@postgres:5432/wager?sslmode=disable" down -all

.PHONY: migrate-version
migrate-version: ## Mostra a versão atual do schema
	$(COMPOSE) run --rm migrate -path=/migrations \
		-database="postgres://wager:wager@postgres:5432/wager?sslmode=disable" version

# -----------------------------------------------------------------------------
# Banco de dados (via container, sem psql instalado no host)
# -----------------------------------------------------------------------------
.PHONY: psql
psql: ## Abre um psql interativo no banco de desenvolvimento
	$(COMPOSE) exec postgres psql -U wager -d wager

# -----------------------------------------------------------------------------
# Filas SQS (via LocalStack)
# -----------------------------------------------------------------------------
.PHONY: queues
queues: ## Lista as filas SQS provisionadas
	$(COMPOSE) exec localstack awslocal sqs list-queues

.PHONY: dlq
dlq: ## Mostra quantas mensagens estão na DLQ de transações
	$(COMPOSE) exec localstack awslocal sqs get-queue-attributes \
		--queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo \
		--attribute-names ApproximateNumberOfMessages

# -----------------------------------------------------------------------------
# Qualidade de código (Go)
# -----------------------------------------------------------------------------
.PHONY: fmt
fmt: ## Formata todo o código Go (gofmt)
	gofmt -l -w .

.PHONY: tidy
tidy: ## Sincroniza go.mod/go.sum
	go mod tidy

.PHONY: vet
vet: ## Análise estática do Go
	go vet ./...

.PHONY: test
test: ## Roda todos os testes
	go test ./...

.PHONY: test-race
test-race: ## Roda os testes com o detector de races (exige CGO_ENABLED=1 e gcc no PATH)
	go test -race ./...

.PHONY: test-race-docker
test-race-docker: ## Roda go test -race em container (dispensa toolchain C no host Windows)
	docker run --rm -v "$(CURDIR)":/app -w /app -e CGO_ENABLED=1 golang:1.27 go test -race ./...

.PHONY: check
check: fmt vet test-race ## Pipeline local: formata, valida e testa com -race

# -----------------------------------------------------------------------------
# Help
# -----------------------------------------------------------------------------
.PHONY: help
help: ## Lista os alvos disponíveis
	@echo "Alvos disponiveis:"
	@echo "  up              Sobe infra local e aplica migrations"
	@echo "  down            Derruba containers (mantem dados)"
	@echo "  clean           Derruba containers e apaga volumes"
	@echo "  ps / logs       Status / logs dos containers"
	@echo "  migrate-up      Aplica migrations pendentes"
	@echo "  migrate-down    Reverte todas as migrations"
	@echo "  migrate-version Mostra a versao atual do schema"
	@echo "  psql            Abre psql no banco de dev"
	@echo "  queues / dlq    Lista filas / inspeciona a DLQ"
	@echo "  fmt / vet       Formata / analisa o codigo"
	@echo "  test / test-race Roda testes (com -race)"
	@echo "  check           fmt + vet + test-race"
