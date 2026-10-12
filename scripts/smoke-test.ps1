<#
=============================================================================
scripts/smoke-test.ps1
=============================================================================
PROPOSITO
	Exercitar a API ponta a ponta contra o ambiente do docker compose:
	autenticacao real no Keycloak, abertura de carteira, apostas, idempotencia,
	rejeicao por saldo, extrato paginado, reconciliacao e ISOLAMENTO ENTRE
	PROVEDORES.

	Serve para duas coisas: (a) provar rapidamente que o sistema funciona apos o
	`docker compose up`; (b) servir de EXEMPLO DE USO da API (secao 15 do spec).

	Equivale, no nivel HTTP, aos criterios de aceite da secao 13.

COMO RODAR
	powershell -ExecutionPolicy Bypass -File scripts/smoke-test.ps1

PRE-REQUISITO
	`docker compose up -d` ja executado (API em http://localhost:8081 e
	Keycloak em http://localhost:8080).

NOTA SOBRE ENCODING (um bug real que este arquivo teve)
	O arquivo original tinha acentos. O Windows PowerShell 5.1 le arquivos .ps1
	sem BOM usando a codepage ANSI do sistema, e os bytes UTF-8 viravam
	sequencias invalidas - quebrando o parser com "MissingEndCurlyBrace".
	Solucao: manter este script em ASCII puro.

OBSERVACAO
	O playerId e gerado a cada execucao, porque (playerId, currency) e unico por
	carteira: reutilizar o mesmo jogador faria a segunda execucao receber 409 -
	comportamento CORRETO do sistema, mas que atrapalharia o teste.
=============================================================================
#>

$ErrorActionPreference = 'Stop'

$ApiBase = 'http://localhost:8081'
$KeycloakBase = 'http://localhost:8080'

$script:failures = 0

function Write-Step([string]$message) {
	Write-Host ""
	Write-Host "=== $message" -ForegroundColor Cyan
}

function Assert-Equal($expected, $actual, [string]$label) {
	if ("$expected" -ne "$actual") {
		Write-Host "  FALHOU  $label - esperado [$expected], obtido [$actual]" -ForegroundColor Red
		$script:failures++
		return
	}
	Write-Host "  OK      $label = $actual" -ForegroundColor Green
}

function Get-Token([string]$clientId, [string]$secret) {
	$response = Invoke-RestMethod -Method Post -Uri "$KeycloakBase/realms/wager/protocol/openid-connect/token" -Body @{ grant_type = 'client_credentials'; client_id = $clientId; client_secret = $secret }
	return $response.access_token
}

function Invoke-Api {
	param(
		[string]$Method,
		[string]$Path,
		[hashtable]$Headers = @{},
		[string]$Body = $null,
		[string]$IdempotencyKey = $null
	)

	$allHeaders = @{}
	foreach ($key in $Headers.Keys) { $allHeaders[$key] = $Headers[$key] }
	if ($IdempotencyKey) { $allHeaders['Idempotency-Key'] = $IdempotencyKey }

	$params = @{
		Method  = $Method
		Uri     = "$ApiBase$Path"
		Headers = $allHeaders
	}
	if ($Body) {
		$params['Body'] = $Body
		$params['ContentType'] = 'application/json'
	}
	return Invoke-RestMethod @params
}

function Get-StatusCode {
	param([scriptblock]$Action)
	try {
		& $Action | Out-Null
		return 200
	} catch {
		if ($_.Exception.Response -ne $null) {
			return [int]$_.Exception.Response.StatusCode
		}
		return 0
	}
}

# -----------------------------------------------------------------------------
# 1) Health checks publicos (secao 12)
# -----------------------------------------------------------------------------
Write-Step "Health checks"

$live = Invoke-RestMethod -Method Get -Uri "$ApiBase/health/live"
Assert-Equal 'alive' $live.status 'liveness'

$ready = Invoke-RestMethod -Method Get -Uri "$ApiBase/health/ready"
Assert-Equal 'ready' $ready.status 'readiness'
Assert-Equal 'ok' $ready.dependencies.postgres 'postgres disponivel'
Assert-Equal 'ok' $ready.dependencies.sqs 'sqs disponivel'

# -----------------------------------------------------------------------------
# 2) Autenticacao (secao 2)
# -----------------------------------------------------------------------------
Write-Step "Autenticacao"

$internalToken = Get-Token 'internal-service' 'dev-internal-service-secret'
$providerAToken = Get-Token 'provider-a' 'dev-provider-a-secret'
$providerBToken = Get-Token 'provider-b' 'dev-provider-b-secret'

Assert-Equal $true ($internalToken.Length -gt 100) 'token do servico interno emitido'
Assert-Equal $true ($providerAToken.Length -gt 100) 'token do provider-a emitido'

$internalHeaders = @{ Authorization = "Bearer $internalToken" }
$providerAHeaders = @{ Authorization = "Bearer $providerAToken" }
$providerBHeaders = @{ Authorization = "Bearer $providerBToken" }

$semToken = Get-StatusCode { Invoke-RestMethod -Method Get -Uri "$ApiBase/wallets/qualquer" }
Assert-Equal 401 $semToken 'requisicao sem token recusada'

# -----------------------------------------------------------------------------
# 3) Abertura de carteira (secao 9) - cenarios da secao 8: saldo de 100.00
# -----------------------------------------------------------------------------
Write-Step "Abertura de carteira com 100.00"

$playerId = [guid]::NewGuid().ToString()
$walletBody = @{ playerId = $playerId; initialBalance = @{ amount = '100.00'; currency = 'BRL' } } | ConvertTo-Json -Compress

$wallet = Invoke-Api -Method Post -Path '/wallets' -Headers $internalHeaders -Body $walletBody
Assert-Equal '100.00' $wallet.balance.amount 'saldo inicial'
Assert-Equal 1 $wallet.version 'versao inicial'
$walletId = $wallet.id

# -----------------------------------------------------------------------------
# 4) Cenario obrigatorio da secao 8: duas apostas de 80.00 sobre saldo de 100.00
# -----------------------------------------------------------------------------
Write-Step "Duas apostas de 80.00 sobre saldo de 100.00 (secao 8)"

function New-BetBody([string]$externalId, [string]$amount) {
	return @{
		providerId            = 'provider-a'
		externalTransactionId = $externalId
		playerId              = $playerId
		walletId              = $walletId
		roundId               = 'round-987'
		gameId                = 'fortune-chimp'
		kind                  = 'BET'
		money                 = @{ amount = $amount; currency = 'BRL' }
	} | ConvertTo-Json -Compress
}

$firstBet = Invoke-Api -Method Post -Path '/wagering/transactions' -Headers $providerAHeaders -Body (New-BetBody 'bet-1' '80.00') -IdempotencyKey 'provider-a:bet-1'

Assert-Equal 'PROCESSED' $firstBet.status 'primeira aposta'
Assert-Equal '20.00' $firstBet.balance.amount 'saldo apos a primeira aposta'
Assert-Equal $false $firstBet.idempotentReplay 'primeira execucao nao e replay'

$secondBet = Invoke-Api -Method Post -Path '/wagering/transactions' -Headers $providerAHeaders -Body (New-BetBody 'bet-2' '80.00') -IdempotencyKey 'provider-a:bet-2'

Assert-Equal 'REJECTED' $secondBet.status 'segunda aposta rejeitada'
Assert-Equal 'BET_INSUFFICIENT_FUNDS' $secondBet.failureCode 'codigo de falha'

# -----------------------------------------------------------------------------
# 5) Idempotencia (secao 9): reenvio da MESMA operacao
# -----------------------------------------------------------------------------
Write-Step "Reenvio da mesma aposta (idempotencia)"

$replay = Invoke-Api -Method Post -Path '/wagering/transactions' -Headers $providerAHeaders -Body (New-BetBody 'bet-1' '80.00') -IdempotencyKey 'provider-a:bet-1'

Assert-Equal $true $replay.idempotentReplay 'reconhecido como replay'
Assert-Equal $firstBet.transactionId $replay.transactionId 'mesmo transactionId'
Assert-Equal '20.00' $replay.balance.amount 'saldo do processamento ORIGINAL'
Assert-Equal 'PROCESSED' $replay.status 'status preservado'

# -----------------------------------------------------------------------------
# 6) Conflito: mesma chave, conteudo diferente (secao 9)
# -----------------------------------------------------------------------------
Write-Step "Conflito de idempotencia (mesma chave, conteudo diferente)"

$conflictStatus = Get-StatusCode {
	Invoke-Api -Method Post -Path '/wagering/transactions' -Headers $providerAHeaders -Body (New-BetBody 'bet-1' '80.01') -IdempotencyKey 'provider-a:bet-1'
}
Assert-Equal 409 $conflictStatus 'conflito de payload'

# -----------------------------------------------------------------------------
# 7) Consulta de carteira, extrato e reconciliacao (secao 9)
# -----------------------------------------------------------------------------
Write-Step "Consulta de carteira, extrato e reconciliacao"

$currentWallet = Invoke-Api -Method Get -Path "/wallets/$walletId" -Headers $internalHeaders
Assert-Equal '20.00' $currentWallet.balance.amount 'saldo atual'

$ledger = Invoke-Api -Method Get -Path "/wallets/$walletId/ledger?limit=10" -Headers $internalHeaders
# Abertura (credito de 100) + UMA aposta processada. A aposta rejeitada NAO gera
# lancamento (secao 6.4) e o replay NAO gera lancamento (secao 9).
Assert-Equal 2 $ledger.entries.Count 'lancamentos no ledger'
Assert-Equal 'CREDIT' $ledger.entries[0].direction 'primeiro lancamento e a abertura'
Assert-Equal 'DEBIT' $ledger.entries[1].direction 'segundo lancamento e a aposta'
Assert-Equal '20.00' $ledger.entries[1].balanceAfter.amount 'saldo encadeado no lancamento'

$reconciliation = Invoke-Api -Method Post -Path "/wallets/$walletId/reconciliation" -Headers $internalHeaders
Assert-Equal $true $reconciliation.consistent 'reconciliacao consistente'
Assert-Equal '0.00' $reconciliation.difference.amount 'diferenca zero'
Assert-Equal '20.00' $reconciliation.calculatedBalance.amount 'saldo reconstruido do ledger'

# -----------------------------------------------------------------------------
# 8) Isolamento entre provedores (secao 2 / secao 13)
# -----------------------------------------------------------------------------
Write-Step "Isolamento entre provedores"

$providerBStatus = Get-StatusCode {
	Invoke-Api -Method Get -Path '/providers/provider-a/wagering/transactions/bet-1' -Headers $providerBHeaders
}
Assert-Equal 403 $providerBStatus 'provider-b nao le transacao do provider-a'

$ownTransaction = Invoke-Api -Method Get -Path '/providers/provider-a/wagering/transactions/bet-1' -Headers $providerAHeaders
Assert-Equal $firstBet.transactionId $ownTransaction.transactionId 'provider-a le a propria transacao'

$providerWalletStatus = Get-StatusCode {
	Invoke-Api -Method Get -Path "/wallets/$walletId" -Headers $providerAHeaders
}
Assert-Equal 403 $providerWalletStatus 'provedor nao acessa carteira diretamente'

# -----------------------------------------------------------------------------
# 9) Rejeicao de OPENING por canal externo (secao 6.3)
# -----------------------------------------------------------------------------
Write-Step "OPENING recusado por canal externo"

$openingBody = @{
	providerId            = 'provider-a'
	externalTransactionId = 'opening-1'
	playerId              = $playerId
	walletId              = $walletId
	roundId               = 'round-987'
	gameId                = 'fortune-chimp'
	kind                  = 'OPENING'
	money                 = @{ amount = '10.00'; currency = 'BRL' }
} | ConvertTo-Json -Compress

$openingStatus = Get-StatusCode {
	Invoke-Api -Method Post -Path '/wagering/transactions' -Headers $providerAHeaders -Body $openingBody -IdempotencyKey 'provider-a:opening-1'
}
Assert-Equal 422 $openingStatus 'OPENING recusado com 422'

# -----------------------------------------------------------------------------
# Resultado
# -----------------------------------------------------------------------------
Write-Host ""
if ($script:failures -gt 0) {
	Write-Host "RESULTADO: $($script:failures) verificacao(oes) falharam" -ForegroundColor Red
	exit 1
}
Write-Host "RESULTADO: todas as verificacoes passaram" -ForegroundColor Green
exit 0
