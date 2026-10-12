# Provisionamento do IdP (Keycloak)

> **Por que este README existe.** O arquivo [`realm-export.json`](realm-export.json)
> é JSON, e **JSON não suporta comentários**. A tentativa inicial de embutir um
> cabeçalho didático como chave `_comment` **quebrou a importação**: o Keycloak 26
> usa Jackson com `FAIL_ON_UNKNOWN_PROPERTIES`, e o log mostrou

```
ERROR: Unrecognized field "_comment" (class org.keycloak.representations.idm.RealmRepresentation)
```

> Então a documentação mora **aqui**, ao lado do arquivo. Lição registrada: nunca
> assuma que um importador tolera campos extras — valide o boot, não a intenção.

---

## O que é provisionado

| Client            | Tipo                   | Claims no token de acesso                            | Para que serve |
|-------------------|------------------------|------------------------------------------------------|----------------|
| `internal-service`| `client_credentials`   | `service_scope=internal`                             | Endpoints internos: `POST /wallets`, `POST /wallets/:id/reconciliation` |
| `provider-a`      | `client_credentials`   | `service_scope=provider`, `provider_id=provider-a`   | Envio de operações (`POST /wagering/transactions`) e consulta das **próprias** transações |
| `provider-b`      | `client_credentials`   | `service_scope=provider`, `provider_id=provider-b`   | Existe para os **testes de isolamento** entre provedores (§2/§13) |

O realm é `wager`, e o usuário de administração é `admin` / `admin` (somente
ambiente local — [`KC_BOOTSTRAP_ADMIN_*`](../../docker-compose.yml)).

## Como as credenciais são usadas

O cabeçalho da requisição leva o token de acesso (`Authorization: Bearer <jwt>`).
A aplicação **não** extrai o `providerId` do corpo da requisição: ele vem da
claim `provider_id` do token. É isso que impede o provedor A de agir em nome do
provedor B — inclusive em replays (§2).

## Obtendo um token (ambiente local)

```sh
# Provider A
curl -s -X POST http://localhost:8080/realms/wager/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=dev-provider-a-secret

# Serviço interno
curl -s -X POST http://localhost:8080/realms/wager/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=internal-service \
  -d client_secret=dev-internal-service-secret
```

A resposta tem `access_token`. Para inspecionar as claims sem dependências,
decodifique o segundo segmento do JWT (base64url) — por exemplo, no PowerShell:

```powershell
$t = (curl.exe -s -X POST http://localhost:8080/realms/wager/protocol/openid-connect/token -d grant_type=client_credentials -d client_id=provider-a -d client_secret=dev-provider-a-secret | ConvertFrom-Json).access_token
$payload = $t.Split('.')[1].Replace('-','+').Replace('_','/')
$pad = 4 - ($payload.Length % 4); if ($pad -ne 4) { $payload += '=' * $pad }
[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($payload)) | ConvertFrom-Json
```

## Detalhes de configuração que importam

- `"sslRequired": "none"` — o ambiente local é HTTP puro; em produção o TLS é
  encerrado no edge e esta opção volta para `external`.
- `"accessTokenLifespan": 300` — tokens curtos; o volume de testes não exige
  refresh, e a expiração é justamente um dos cenários do §13.
- `protocolMappers` do tipo `oidc-hardcoded-claim-mapper` — injetam as claims
  sem exigir usuários reais, mantendo `client_credentials` autocontido.
- `standardFlowEnabled: false` e `directAccessGrantsEnabled: false` — o desafio
  não prevê fluxo de usuário nem senha (§2: cadastro de senhas e emissão própria
  de tokens estão fora de escopo).
