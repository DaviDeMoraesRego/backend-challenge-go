// Dois processos (api, worker) com perfis de recurso e escala distintos: a API usa
// conexões HTTP curtas, o worker conexões longas com broker e banco. Binários separados
// atendem ao §8 ("três processos independentes"). O worker roda três laços — consumer
// SQS (§10), relay do outbox (§11) e worker de referências (§7) — compartilhando o mesmo
// pool: a serialização por carteira é feita por locks no banco, não por isolamento.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/fx"

	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/inbound/sqsworker"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/config"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/metrics"
)

// NewAPIApp monta o processo da API. A configuração chega já carregada (fx.Supply):
// assim existe uma única leitura/validação, e não uma por módulo.
func NewAPIApp(cfg config.Config) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		ObservabilityModule,
		MetricsModule,
		PostgresModule,
		SQSClientModule, // readiness verifica o broker (§12)
		KeycloakModule,
		CoreUseCaseModule,
		HTTPModule,
	)
}

// NewWorkerApp monta o processo dos workers. Não inclui KeycloakModule: o worker não
// atende HTTP nem valida token — o controle de acesso à mensageria é do broker (§2).
func NewWorkerApp(cfg config.Config) *fx.App {
	return fx.New(
		fx.Supply(cfg),
		ObservabilityModule,
		MetricsModule,
		PostgresModule,
		SQSClientModule,
		SQSConsumerModule,
		CoreUseCaseModule,
		WorkerUseCaseModule,
		WorkerModule,
	)
}

// WorkerModule sobe os laços do worker no ciclo de vida.
var WorkerModule = fx.Module("workers",
	fx.Invoke(StartWorkers),
)

// StartWorkers sobe os três laços e garante o encerramento ordenado (§4): cancel()
// sinaliza a todos pararem de buscar trabalho novo; o WaitGroup espera o trabalho em
// andamento terminar; só então o hook devolve e o Fx fecha o pool (registrado antes).
// O select com stopCtx.Done() impede um laço travado de segurar o shutdown — o trabalho
// não concluído é retomado pela próxima instância (mensagem volta à fila, pendência no banco).
func StartWorkers(
	lc fx.Lifecycle,
	consumer *sqsworker.Consumer,
	relay ports.RelayOutbox,
	resolver ports.ResolvePendingReferences,
	cfg config.Config,
	logger *slog.Logger,
	collector *metrics.Metrics,
) {
	// Contexto de vida do worker: cancelado no shutdown.
	workerCtx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			logger.Info("iniciando workers",
				slog.Duration("outbox_interval", cfg.OutboxRelayInterval),
				slog.Duration("pending_reference_interval", cfg.PendingReferenceScanInterval),
			)

			// 1) Consumidor SQS.
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := consumer.Run(workerCtx); err != nil {
					logger.Error("consumidor sqs encerrou com erro", slog.String("error", err.Error()))
				}
			}()

			// 2) Relay do outbox. O caso de uso devolve um resumo da rodada (insumo das
			// métricas do §12); logar só quando há atividade evita poluir o log.
			wg.Add(1)
			go func() {
				defer wg.Done()
				runPeriodicLoop(workerCtx, "outbox relay", cfg.OutboxRelayInterval, logger,
					func(ctx context.Context) error {
						result, err := relay.Execute(ctx)
						if err != nil {
							return err
						}
						collector.ObserveOutboxRelay(result.Claimed, result.Published, result.Rescheduled, result.Failed)
						collector.ObserveOutboxLag(result.MaxLag)

						if result.Claimed > 0 {
							logger.Info("rodada do outbox relay",
								slog.Int("claimed", result.Claimed),
								slog.Int("published", result.Published),
								slog.Int("rescheduled", result.Rescheduled),
								slog.Int("failed", result.Failed),
							)
						}
						return nil
					})
			}()

			// 3) Worker de referências pendentes.
			wg.Add(1)
			go func() {
				defer wg.Done()
				runPeriodicLoop(workerCtx, "reference worker", cfg.PendingReferenceScanInterval, logger,
					func(ctx context.Context) error {
						result, err := resolver.Execute(ctx)
						if err != nil {
							return err
						}
						collector.ObserveReferences(result.Resolved, result.Rescheduled, result.Rejected, result.Failed)

						if result.Scanned > 0 {
							logger.Info("rodada do worker de referencias",
								slog.Int("scanned", result.Scanned),
								slog.Int("resolved", result.Resolved),
								slog.Int("rescheduled", result.Rescheduled),
								slog.Int("rejected", result.Rejected),
								slog.Int("failed", result.Failed),
							)
						}
						return nil
					})
			}()

			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			logger.Info("encerrando workers: parando de buscar trabalho novo")
			cancel()

			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				logger.Info("workers encerrados com o trabalho em andamento concluido")
				return nil
			case <-stopCtx.Done():
				// Prazo estourado: encerrar é melhor que ficar preso. O trabalho não
				// concluído não se perde (a mensagem volta à fila; a pendência fica no banco).
				return fmt.Errorf("workers: encerramento excedeu o prazo: %w", stopCtx.Err())
			}
		},
	})
}

// runPeriodicLoop executa `run` a cada `interval` até o contexto ser cancelado. O
// defer Stop() evita goroutine leak; o select com o contexto faz o cancelamento
// interromper a espera, não só a próxima rodada.
func runPeriodicLoop(
	ctx context.Context,
	name string,
	interval time.Duration,
	logger *slog.Logger,
	run func(context.Context) error,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Cada rodada tem seu próprio prazo: uma rodada lenta não atrasa as seguintes
			// nem trava o shutdown.
			roundCtx, cancel := context.WithTimeout(ctx, interval)
			err := run(roundCtx)
			cancel()

			if err != nil {
				// Falha de rodada é registrada; a próxima tentativa vem no próximo tique.
				logger.Warn("rodada do worker falhou",
					slog.String("worker", name),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}
