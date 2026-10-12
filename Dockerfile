# =============================================================================
# Dockerfile
# =============================================================================
# PROPÓSITO
#   Construir a imagem dos DOIS binários (`api` e `worker`) a partir de um único
#   Dockerfile, com build multi-stage e runtime mínimo.
#
# CAMADA DA ARQUITETURA HEXAGONAL
#   infrastructure (empacotamento).
#
# POR QUE ASSIM
#   1. MULTI-STAGE: o compilador e o código-fonte ficam na imagem de BUILD e não
#      aparecem na imagem final. Resultado: imagem de runtime na casa dos MB, sem
#      toolchain — menos superfície para vulnerabilidade.
#   2. `ARG TARGET`: o mesmo Dockerfile produz os dois binários. Menos arquivos
#      para manter sincronizados; um `--build-arg TARGET=worker` muda o alvo.
#   3. `CGO_ENABLED=0`: binário estático. É o que permite usar a imagem
#      `distroless/static`, que não tem libc — a menor base prática.
#   4. `-trimpath` e `-ldflags="-s -w"`: removem caminhos locais do binário
#      (reprodutibilidade) e a tabela de símbolos/depuração (tamanho).
#   5. USUÁRIO NÃO-ROOT: a tag `nonroot` já roda como uid 65532. Um processo
#      comprometido não tem privilégio para instalar nem alterar o sistema.
#   6. SEM SHELL NA IMAGEM FINAL: `distroless` não traz `/bin/sh`. Isso elimina
#      uma classe inteira de ataques de execução remota — ao custo de não poder
#      entrar no container. Para depuração, use a imagem de build.
#
# VALIDAÇÃO LOCAL
#   docker build --build-arg TARGET=api -t wager-api .
#   docker build --build-arg TARGET=worker -t wager-worker .
#
# ARMADILHA CLÁSSICA
#   Copiar o código ANTES do `go mod download` invalidaria o cache de camadas a
#   cada alteração em um `.go` qualquer. Copiando primeiro só `go.mod`/`go.sum`,
#   o download das dependências é reaproveitado enquanto elas não mudarem.
# =============================================================================

# -----------------------------------------------------------------------------
# Estágio 1: BUILD
# -----------------------------------------------------------------------------
FROM golang:1.27-alpine AS build

# `TARGET` escolhe o binário: "api" ou "worker".
ARG TARGET=api

WORKDIR /src

# 1) Dependências primeiro (camada cacheável).
COPY go.mod go.sum ./
RUN go mod download

# 2) Código.
COPY . .

# 3) Compilação estática e enxuta.
#    `-trimpath` remove o caminho de build do binário; `-s -w` retira símbolos.
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

# -----------------------------------------------------------------------------
# Estágio 2: RUNTIME
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

# Copiamos APENAS o binário. Nada de código-fonte, toolchain ou gerenciador de
# pacotes na imagem final.
COPY --from=build /out/app /app

# A tag `nonroot` já define uid/gid 65532; declarar explicitamente documenta a
# intenção e protege contra troca de base.
USER nonroot:nonroot

# `ENTRYPOINT` em forma de exec (não shell): o processo vira PID 1 e recebe
# SIGTERM diretamente — o que o shutdown gracioso do §4 exige. Com a forma
# shell (`CMD app`), o sinal iria para o shell e o processo seria morto à força.
ENTRYPOINT ["/app"]
