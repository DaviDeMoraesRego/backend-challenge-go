# Imagem dos DOIS binários (`api` e `worker`) a partir de um único Dockerfile.
#
# - Multi-stage: toolchain só no build; runtime distroless (MB, sem shell, nonroot).
# - `ARG TARGET` escolhe o binário (`--build-arg TARGET=worker`).
# - `CGO_ENABLED=0` -> binário estático (exigido por distroless/static, sem libc).
# - `-trimpath -ldflags="-s -w"`: reprodutibilidade e menos símbolos.
# - Copiar só `go.mod`/`go.sum` antes do código mantém o cache de `go mod download`.
# - ENTRYPOINT exec: o processo é PID 1 e recebe SIGTERM direto (shutdown gracioso).

# Estágio 1: BUILD
FROM golang:1.27-alpine AS build

ARG TARGET=api

WORKDIR /src

# Dependências primeiro (camada cacheável).
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

# Estágio 2: RUNTIME
FROM gcr.io/distroless/static-debian12:nonroot

# Só o binário: nada de código-fonte, toolchain ou gerenciador de pacotes.
COPY --from=build /out/app /app

USER nonroot:nonroot

# Forma exec: o processo vira PID 1 e recebe SIGTERM direto (na forma shell, o
# sinal iria para o shell e o processo seria morto à força).
ENTRYPOINT ["/app"]
