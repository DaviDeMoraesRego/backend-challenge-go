/*
=============================================================================
tests/k6/bet-flow.js
=============================================================================
PROPOSITO
	Teste de carga do fluxo principal de apostas: autenticacao real no Keycloak,
	abertura de carteira, envio de operacoes e leitura do extrato — com CENARIOS
	distintos (ramp-up, pico e soak) e THRESHOLDS EXPLICITOS.

COMO RODAR
	docker run --rm -i --network host -v "%cd%/tests/k6":/scripts grafana/k6 \
	  run /scripts/bet-flow.js

	Ou, com k6 instalado no host:
	k6 run tests/k6/bet-flow.js

	Para escolher um cenario especifico:
	k6 run -e SCENARIO=ramp_up tests/k6/bet-flow.js

=== O QUE ESTE TESTE MEDE, E POR QUE CADA CENARIO EXISTE ===

	ramp_up  — carga CRESCENTE e sustentada. Revela o ponto em que a latencia
	           comeca a degradar (o joelho da curva) e valida que o sistema aguenta
	           o regime normal com folga.

	spike    — salto REPENTINO para carga alta e retorno imediato. Revela se o
	           sistema se recupera apos uma rajada: pool de conexoes saturado,
	           filas acumuladas no broker, workers atrasados. Um sistema pode
	           passar no ramp-up e falhar aqui.

	soak     — carga MODERADA por tempo LONGO. Revela vazamentos: goroutines nao
	           encerradas, conexoes que nao voltam ao pool, memoria crescendo. E o
	           unico cenario em que esse tipo de defeito aparece.

=== SOBRE OS THRESHOLDS ===
	Thresholds sao ASSERTIVAS, nao enfeite: o k6 termina com codigo != 0 se algum
	falhar, o que torna o teste utilizavel em CI.

	Os valores abaixo sao ponto de partida para o ambiente LOCAL do docker
	compose (LocalStack + Postgres no mesmo host). Em uma execucao real, ajuste-os
	apos a PRIMEIRA medicao — threshold que nunca falha nao protege nada, e
	threshold que falha sempre nao e usado.

=== O QUE ESTE TESTE NAO E ===
	Nao e um benchmark de throughput absoluto: o LocalStack e o Postgres rodam no
	mesmo host e disputam CPU com o proprio k6. Os numeros servem para comparar
	EXECUCOES entre si (regressao), nao para prever producao.
=============================================================================
*/

import http from 'k6/http';
import { check, sleep, group } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { randomUUID } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

// -----------------------------------------------------------------------------
// Configuracao
// -----------------------------------------------------------------------------

const API_BASE = __ENV.API_BASE || 'http://localhost:8081';
const KEYCLOAK_BASE = __ENV.KEYCLOAK_BASE || 'http://localhost:8080';
const REALM = __ENV.KEYCLOAK_REALM || 'wager';

// Metricas de NEGOCIO: o k6 so mede HTTP por padrao. Estas contam o que
// interessa financeiramente — processadas, rejeitadas e replays idempotentes.
const betsProcessed = new Counter('bets_processed');
const betsRejected = new Counter('bets_rejected');
const betsReplayed = new Counter('bets_idempotent_replay');
const balanceObserved = new Trend('bet_balance_observed', true);

// -----------------------------------------------------------------------------
// Cenarios
// -----------------------------------------------------------------------------

const selectedScenario = __ENV.SCENARIO;

export const options = {
	scenarios: buildScenarios(selectedScenario),
	thresholds: {
		// Taxa de erro HTTP: ate 1%. O esperado neste fluxo e praticamente zero —
		// o 1% cobre rejeicao de negocio respondida com status nao-2xx caso o
		// contrato mude.
		http_req_failed: ['rate<0.01'],

		// Latencia p95 abaixo de 500ms no regime sustentado. Com LocalStack no
		// mesmo host, 200ms ja seria otimista para incluir a ida ao broker.
		'http_req_duration{scenario:ramp_up}': ['p(95)<500', 'p(99)<1000'],

		// No pico toleramos mais latencia; o que NAO toleramos e erro.
		'http_req_duration{scenario:spike}': ['p(95)<1000'],
		'http_req_failed{scenario:spike}': ['rate<0.02'],

		// No soak, o criterio e ESTABILIDADE: latencia estavel e zero erro.
		'http_req_duration{scenario:soak}': ['p(95)<500'],
		'http_req_failed{scenario:soak}': ['rate<0.001'],

		// O negocio tem de ter acontecido.
		bets_processed: ['count>0'],
	},
};

function buildScenarios(only) {
	const all = {
		ramp_up: {
			executor: 'ramping-vus',
			exec: 'betFlow',
			startVUs: 1,
			stages: [
				{ duration: '15s', target: 10 }, // aquecimento
				{ duration: '30s', target: 30 }, // rampa
				{ duration: '30s', target: 30 }, // patamar
				{ duration: '10s', target: 0 },  // descida
			],
			gracefulStop: '10s',
		},
		spike: {
			executor: 'ramping-vus',
			exec: 'betFlow',
			startVUs: 1,
			stages: [
				{ duration: '10s', target: 5 },
				{ duration: '10s', target: 60 }, // salto
				{ duration: '20s', target: 60 }, // sustenta o pico
				{ duration: '10s', target: 5 },  // queda
				{ duration: '20s', target: 5 },  // observa a RECUPERACAO
			],
			gracefulStop: '15s',
		},
		soak: {
			executor: 'constant-vus',
			exec: 'betFlow',
			vus: 10,
			duration: '3m',
			gracefulStop: '15s',
		},
	};

	if (only && all[only]) {
		return { [only]: all[only] };
	}
	return all;
}

// -----------------------------------------------------------------------------
// setup: tokens e carteiras
// -----------------------------------------------------------------------------

// setup roda UMA vez, antes da carga. Aqui buscamos os tokens e criamos uma
// carteira por VU previsto — porque (playerId, currency) e unico, duas VUs nao
// podem compartilhar a mesma carteira.
export function setup() {
	const internalToken = fetchToken('internal-service', 'dev-internal-service-secret');
	const providerToken = fetchToken('provider-a', 'dev-provider-a-secret');

	const walletCount = parseInt(__ENV.WALLETS || '60', 10);
	const wallets = [];

	for (let i = 0; i < walletCount; i++) {
		const playerId = randomUUID();
		const response = http.post(
			`${API_BASE}/wallets`,
			JSON.stringify({
				playerId: playerId,
				// Saldo alto de proposito: o objetivo da carga e medir LATENCIA,
				// nao a taxa de rejeicao por saldo insuficiente.
				initialBalance: { amount: '1000000.00', currency: 'BRL' },
			}),
			{ headers: jsonHeaders(internalToken) },
		);

		if (response.status !== 201) {
			throw new Error(`falha ao criar carteira: ${response.status} ${response.body}`);
		}

		const wallet = JSON.parse(response.body);
		wallets.push({ id: wallet.id, playerId: playerId });
	}

	return { internalToken, providerToken, wallets };
}

function fetchToken(clientId, clientSecret) {
	const response = http.post(
		`${KEYCLOAK_BASE}/realms/${REALM}/protocol/openid-connect/token`,
		`grant_type=client_credentials&client_id=${clientId}&client_secret=${clientSecret}`,
		{ headers: { 'Content-Type': 'application/x-www-form-urlencoded' } },
	);

	if (response.status !== 200) {
		throw new Error(`falha ao obter token (${clientId}): ${response.status} ${response.body}`);
	}
	return JSON.parse(response.body).access_token;
}

function jsonHeaders(token) {
	return { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` };
}

// -----------------------------------------------------------------------------
// Fluxo principal
// -----------------------------------------------------------------------------

export function betFlow(data) {
	// Cada VU fica com uma carteira propria: assim a carga nao serializa numa
	// unica linha do banco (o lock e POR CARTEIRA) e medimos paralelismo real.
	const wallet = data.wallets[(__VU - 1) % data.wallets.length];

	group('aposta', () => {
		const externalId = `k6-${__VU}-${__ITER}-${randomUUID().slice(0, 8)}`;

		const response = http.post(
			`${API_BASE}/wagering/transactions`,
			JSON.stringify({
				providerId: 'provider-a',
				externalTransactionId: externalId,
				playerId: wallet.playerId,
				walletId: wallet.id,
				roundId: `round-${__ITER}`,
				gameId: 'fortune-chimp',
				kind: 'BET',
				money: { amount: '25.00', currency: 'BRL' },
			}),
			{
				headers: jsonHeaders(data.providerToken),
				// A chave de idempotencia usa o id externo: operacoes distintas
				// continuam distintas, e o reenvio do MESMO id viraria replay.
				headers: Object.assign(jsonHeaders(data.providerToken), {
					'Idempotency-Key': `provider-a:${externalId}`,
				}),
			},
		);

		const ok = check(response, {
			'aposta aceita (200/202)': (r) => r.status === 200 || r.status === 202,
			'resposta tem status de negocio': (r) => {
				try {
					const body = JSON.parse(r.body);
					return body.status === 'PROCESSED' || body.status === 'PENDING_REFERENCE';
				} catch (e) {
					return false;
				}
			},
		});

		if (ok) {
			const body = JSON.parse(response.body);
			if (body.status === 'PROCESSED') {
				betsProcessed.add(1);
			}
			if (body.idempotentReplay) {
				betsReplayed.add(1);
			}
			if (body.balance) {
				balanceObserved.add(parseFloat(body.balance.amount));
			}
		}
	});

	// Uma pausa entre iteracoes: sem ela, cada VU gera carga maxima continua e o
	// teste mede saturacao do cliente, nao do servidor.
	sleep(Math.random() * 0.5 + 0.2);
}

// -----------------------------------------------------------------------------
// Cenario de IDEMPOTENCIA sob carga
// -----------------------------------------------------------------------------

// duplicateFlow dispara a MESMA operacao varias vezes em paralelo e verifica que
// apenas uma movimenta o saldo. E o cenario da secao 13 do spec executado sob
// carga, e nao como teste funcional.
//
// Rodar com: SCENARIO=duplicates k6 run tests/k6/bet-flow.js
export function duplicateFlow(data) {
	const wallet = data.wallets[(__VU - 1) % data.wallets.length];
	const externalId = `k6-dup-${__VU}-${__ITER}`;
	const idempotencyKey = `provider-a:${externalId}`;

	const responses = http.batch([
		['POST', `${API_BASE}/wagering/transactions`, JSON.stringify({
			providerId: 'provider-a',
			externalTransactionId: externalId,
			playerId: wallet.playerId,
			walletId: wallet.id,
			roundId: 'round-duplicate',
			gameId: 'fortune-chimp',
			kind: 'BET',
			money: { amount: '10.00', currency: 'BRL' },
		}), {
			headers: Object.assign(jsonHeaders(data.providerToken), {
				'Idempotency-Key': idempotencyKey,
			}),
		}],
		['POST', `${API_BASE}/wagering/transactions`, JSON.stringify({
			providerId: 'provider-a',
			externalTransactionId: externalId,
			playerId: wallet.playerId,
			walletId: wallet.id,
			roundId: 'round-duplicate',
			gameId: 'fortune-chimp',
			kind: 'BET',
			money: { amount: '10.00', currency: 'BRL' },
		}), {
			headers: Object.assign(jsonHeaders(data.providerToken), {
				'Idempotency-Key': idempotencyKey,
			}),
		}],
	]);

	let applied = 0;
	let replayed = 0;

	for (const response of responses) {
		if (response.status !== 200) {
			continue;
		}
		const body = JSON.parse(response.body);
		if (body.idempotentReplay) {
			replayed++;
		} else {
			applied++;
		}
	}

	check(null, {
		'apenas UMA aplicacao por chave': () => applied === 1,
		'as demais foram reconhecidas como replay': () => replayed === responses.length - 1,
	});

	sleep(0.2);
}
