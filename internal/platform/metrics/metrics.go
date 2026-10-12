// Métricas do §12. Tipos: COUNTER (eventos, só cresce), GAUGE (estado, sobe e desce) e
// HISTOGRAM (latência em buckets, de onde saem p50/p95/p99). Registry próprio (não o
// global do Prometheus) para isolar construções — inclusive em testes.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics reúne os coletores e o registry onde eles vivem.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	operations     *prometheus.CounterVec
	replays        prometheus.Counter
	conflicts      prometheus.Counter
	outboxRelay    *prometheus.CounterVec
	outboxPending  prometheus.Gauge
	outboxLag      prometheus.Gauge
	references     *prometheus.CounterVec
	reconcileDrift prometheus.Counter
}

// New cria as métricas e as registra num registry próprio.
func New() *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		registry: registry,

		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_http_requests_total",
			Help: "Total de requisicoes HTTP por metodo, rota e status.",
		}, []string{"method", "route", "status"}),

		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "wager_http_request_duration_seconds",
			Help: "Latencia das requisicoes HTTP em segundos.",
			// Buckets calibrados para a faixa que importa (5ms a 1s) numa API financeira.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		}, []string{"method", "route"}),

		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_operations_total",
			Help: "Operacoes processadas por tipo e status (§12: resultados por status).",
		}, []string{"kind", "status"}),

		replays: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_idempotent_replays_total",
			Help: "Reenvios reconhecidos como replay idempotente (§12: duplicatas).",
		}),

		conflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_idempotency_conflicts_total",
			Help: "Conflitos de idempotencia devolvidos ao cliente (§9).",
		}),

		outboxRelay: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_outbox_relay_total",
			Help: "Resultado das publicacoes da outbox (§12: retries e falhas).",
		}, []string{"outcome"}),

		outboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wager_outbox_pending",
			Help: "Eventos reservados na ultima rodada do relay.",
		}),

		outboxLag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "wager_outbox_lag_seconds",
			Help: "Tempo entre a ocorrencia do evento e a sua publicacao (§12: atraso da outbox).",
		}),

		references: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wager_pending_references_total",
			Help: "Resultado das rodadas do worker de referencias (§7).",
		}, []string{"outcome"}),

		reconcileDrift: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wager_reconciliation_divergences_total",
			Help: "Reconciliacoes que encontraram divergencia (§12).",
		}),
	}

	// Registro explícito de cada coletor: um esquecimento faria a métrica não aparecer,
	// sem erro — o pior tipo de falha em telemetria.
	registry.MustRegister(
		m.httpRequests,
		m.httpDuration,
		m.operations,
		m.replays,
		m.conflicts,
		m.outboxRelay,
		m.outboxPending,
		m.outboxLag,
		m.references,
		m.reconcileDrift,
	)
	// Métricas de processo (CPU, memória, goroutines) do coletor padrão do runtime.
	registry.MustRegister(prometheus.NewGoCollector())

	return m
}

// Handler devolve o endpoint `/metrics`.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveHTTP registra a requisição. `route` é o PADRÃO (ex.: "/wallets/{walletId}"),
// nunca o caminho concreto — o caminho real explodiria a cardinalidade.
func (m *Metrics) ObserveHTTP(method, route string, status int, duration time.Duration) {
	m.httpRequests.WithLabelValues(method, route, http.StatusText(status)).Inc()
	m.httpDuration.WithLabelValues(method, route).Observe(duration.Seconds())
}

// ObserveOperation registra o resultado de uma operação.
func (m *Metrics) ObserveOperation(kind, status string) {
	m.operations.WithLabelValues(kind, status).Inc()
}

// ObserveReplay registra um reenvio idempotente.
func (m *Metrics) ObserveReplay() { m.replays.Inc() }

// ObserveConflict registra um conflito de idempotência.
func (m *Metrics) ObserveConflict() { m.conflicts.Inc() }

// ObserveOutboxRelay registra o resumo de uma rodada do relay.
func (m *Metrics) ObserveOutboxRelay(claimed, published, rescheduled, failed int) {
	m.outboxPending.Set(float64(claimed))
	m.outboxRelay.WithLabelValues("published").Add(float64(published))
	m.outboxRelay.WithLabelValues("rescheduled").Add(float64(rescheduled))
	m.outboxRelay.WithLabelValues("failed").Add(float64(failed))
}

// ObserveOutboxLag registra quanto tempo um evento esperou para ser publicado.
func (m *Metrics) ObserveOutboxLag(lag time.Duration) {
	if lag < 0 {
		lag = 0
	}
	m.outboxLag.Set(lag.Seconds())
}

// ObserveReferences registra o resumo de uma rodada do worker de referências.
func (m *Metrics) ObserveReferences(resolved, rescheduled, rejected, failed int) {
	m.references.WithLabelValues("resolved").Add(float64(resolved))
	m.references.WithLabelValues("rescheduled").Add(float64(rescheduled))
	m.references.WithLabelValues("rejected").Add(float64(rejected))
	m.references.WithLabelValues("failed").Add(float64(failed))
}

// ObserveReconciliationDivergence registra uma divergência encontrada.
func (m *Metrics) ObserveReconciliationDivergence() { m.reconcileDrift.Inc() }
