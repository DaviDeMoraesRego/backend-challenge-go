// Composition root: os fx.Module ligam adapters concretos às portas. fx.Provide
// declara; fx.Invoke força a construção. fx.As vincula concreto a interface (o Fx não
// infere essa ligação). fx.Lifecycle registra OnStop, executados em ordem inversa à de
// registro (consumidores antes do recurso, §4). Há dois módulos de caso de uso porque o
// relay do outbox depende de um publisher (SQS) que só existe no worker.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/inbound/httpapi"
	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/inbound/sqsworker"
	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/outbound/keycloak"
	"github.com/davidemoraesrego/backend-challenge-go/internal/adapters/outbound/postgres"
	outboundsqs "github.com/davidemoraesrego/backend-challenge-go/internal/adapters/outbound/sqs"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/ports"
	"github.com/davidemoraesrego/backend-challenge-go/internal/application/usecase"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/awsclient"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/clock"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/config"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/idgen"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/metrics"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/observability"
)

// ObservabilityModule fornece o logger, o relógio, o gerador de ID e os health checks.
var ObservabilityModule = fx.Module("observability",
	fx.Provide(
		func(cfg config.Config) *slog.Logger {
			return observability.NewLogger(observability.LoggerConfig{
				Level:       cfg.LogLevel,
				ServiceName: "wager-backend",
			})
		},
		// Relógio e gerador de ID: concretos que satisfazem portas do domínio.
		fx.Annotate(clock.New, fx.As(new(ports.Clock))),
		fx.Annotate(idgen.New, fx.As(new(ports.IDGenerator))),
	),
)

// PostgresModule fornece o pool e a unidade de trabalho.
var PostgresModule = fx.Module("postgres",
	fx.Provide(NewPostgresPool),
	// O construtor devolve o concreto e os casos de uso pedem ports.UnitOfWork; sem o
	// fx.As o Fx falha com "missing type: ports.UnitOfWork".
	fx.Provide(fx.Annotate(postgres.NewUnitOfWork, fx.As(new(ports.UnitOfWork)))),
)

// NewPostgresPool cria o pool e registra o fechamento no ciclo de vida. O OnStop é
// registrado na construção (antes dos servidores/workers que o usam): como o Fx executa
// os hooks em ordem inversa, o pool fecha por último (§4). Registrar na ordem errada
// produziria "pool is closed" durante o shutdown.
func NewPostgresPool(cfg config.Config, lc fx.Lifecycle, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(context.Background(), postgres.PoolConfig{
		URL:      cfg.DatabaseURL,
		MaxConns: cfg.DatabaseMaxConns,
	})
	if err != nil {
		return nil, err
	}

	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			logger.Info("fechando pool de conexoes")
			// Close espera as conexões em uso voltarem (encerramento gracioso).
			pool.Close()
			return nil
		},
	})

	return pool, nil
}

// SQSClientModule fornece o cliente SQS e o health check do broker. Entra nos dois
// processos: a API precisa do health check (§12), o worker do cliente para consumir e
// publicar. O Fx só constrói o que for pedido — na API, publisher e consumer não existem.
var SQSClientModule = fx.Module("sqs-client",
	fx.Provide(NewSQSClient),
	fx.Provide(NewSQSHealthCheck),
)

// SQSConsumerModule fornece o consumidor — apenas para o processo worker.
var SQSConsumerModule = fx.Module("sqs-consumer",
	fx.Provide(NewSQSConsumer),
)

// NewSQSClient cria o cliente SQS a partir da configuração.
func NewSQSClient(cfg config.Config) (SQSPublisherClient, error) {
	client, err := awsclient.NewSQSClient(context.Background(), cfg.SQSRegion, cfg.SQSEndpoint)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// SQSPublisherClient reúne as três interfaces que o cliente SQS atende (publicar,
// consumir, checar saúde). O SDK entrega um único tipo que satisfaz as três; declarar a
// união dá ao Fx um tipo único e permite injetar a mesma instância — não três clientes.
type SQSPublisherClient interface {
	outboundsqs.PublisherClient
	sqsworker.Client
	observability.SQSAttributeClient
}

// NewEventPublisher monta o publisher apontando para a fila de eventos.
func NewEventPublisher(client SQSPublisherClient, cfg config.Config) *outboundsqs.Publisher {
	return outboundsqs.NewPublisher(client, cfg.SQSEventsQueueURL)
}

// NewSQSConsumer monta o consumidor apontando para a fila de entrada.
func NewSQSConsumer(client SQSPublisherClient, place ports.PlaceWagerOperation, cfg config.Config, logger *slog.Logger) *sqsworker.Consumer {
	return sqsworker.NewConsumer(client, place, sqsworker.Config{
		QueueURL:          cfg.SQSQueueURL,
		ConsumerName:      cfg.SQSConsumerName,
		MaxMessages:       int32(cfg.SQSMaxMessages),
		WaitTimeSeconds:   int32(cfg.SQSWaitTimeSeconds),
		VisibilityTimeout: int32(cfg.SQSVisibilityTimeout),
	}, logger)
}

// NewSQSHealthCheck monta a verificação de disponibilidade do broker (§12).
func NewSQSHealthCheck(client SQSPublisherClient, cfg config.Config) *observability.SQSHealth {
	return observability.NewSQSHealth(client, cfg.SQSQueueURL)
}

// KeycloakModule fornece o validador de tokens (§2).
var KeycloakModule = fx.Module("keycloak",
	fx.Provide(NewTokenVerifier),
)

// NewTokenVerifier cria o validador JWKS/RS256.
func NewTokenVerifier(cfg config.Config) (*keycloak.Verifier, error) {
	return keycloak.NewVerifier(cfg.KeycloakJWKSURL, cfg.KeycloakIssuer, nil)
}

// CoreUseCaseModule fornece os casos de uso usados por API e worker.
var CoreUseCaseModule = fx.Module("usecases-core",
	// fx.As é obrigatório: os construtores devolvem tipos concretos e os consumidores
	// pedem interfaces — o Fx não infere essa ligação.
	fx.Provide(
		fx.Annotate(usecase.NewOpenWalletService, fx.As(new(ports.OpenWallet))),
		fx.Annotate(usecase.NewPlaceWagerOperationService, fx.As(new(ports.PlaceWagerOperation))),
		fx.Annotate(usecase.NewGetWalletService, fx.As(new(ports.GetWallet))),
		fx.Annotate(usecase.NewGetWalletLedgerService, fx.As(new(ports.GetWalletLedger))),
		fx.Annotate(usecase.NewGetWagerTransactionService, fx.As(new(ports.GetWagerTransaction))),
		fx.Annotate(usecase.NewGetWagerTransactionByExternalIDService, fx.As(new(ports.GetWagerTransactionByExternalID))),
		fx.Annotate(usecase.NewReconcileWalletService, fx.As(new(ports.ReconcileWallet))),
	),
	// A configuração do caso de uso é um dado de política: fornecemos como valor.
	fx.Provide(func(cfg config.Config) usecase.PlaceWagerOperationConfig {
		return usecase.PlaceWagerOperationConfig{
			PendingReferenceBaseBackoff: cfg.PendingReferenceBaseBackoff,
		}
	}),
)

// WorkerUseCaseModule fornece os casos de uso de processo. Separado do core porque
// depende de ports.EventPublisher (SQS), que só existe no worker.
var WorkerUseCaseModule = fx.Module("usecases-worker",
	fx.Provide(
		fx.Annotate(NewEventPublisher, fx.As(new(ports.EventPublisher))),
		fx.Annotate(usecase.NewRelayOutboxService, fx.As(new(ports.RelayOutbox))),
		fx.Annotate(usecase.NewResolvePendingReferencesService, fx.As(new(ports.ResolvePendingReferences))),
	),
	fx.Provide(func(cfg config.Config) usecase.RelayOutboxConfig {
		return usecase.RelayOutboxConfig{
			BatchSize:   cfg.OutboxRelayBatchSize,
			Lease:       cfg.OutboxRelayLease,
			BaseBackoff: cfg.PendingReferenceBaseBackoff,
			MaxAttempts: cfg.OutboxRelayMaxAttempts,
		}
	}),
	fx.Provide(func(cfg config.Config) usecase.ResolvePendingReferencesConfig {
		return usecase.ResolvePendingReferencesConfig{
			BaseBackoff: cfg.PendingReferenceBaseBackoff,
			MaxAttempts: cfg.PendingReferenceMaxAttempts,
		}
	}),
)

// HTTPModule fornece handlers e router, e sobe o servidor no ciclo de vida.
var HTTPModule = fx.Module("http",
	fx.Provide(httpapi.NewHandlers),
	fx.Provide(NewRouter),
	fx.Provide(NewHealthChecks),
	fx.Invoke(StartHTTPServer),
)

// MetricsModule expõe o endpoint de métricas (§12).
var MetricsModule = fx.Module("metrics",
	fx.Provide(metrics.New),
	fx.Invoke(StartMetricsServer),
)

// StartMetricsServer sobe o servidor de métricas num endereço separado da API: /metrics
// não deve ficar no mesmo port que o tráfego de negócio — separar endpoints é
// pré-requisito para políticas de rede diferentes.
func StartMetricsServer(lc fx.Lifecycle, collector *metrics.Metrics, cfg config.Config, logger *slog.Logger) {
	if cfg.MetricsAddr == "" {
		// Endereço vazio desliga o servidor: útil em testes de composição.
		return
	}

	server := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           collector.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			listener, err := net.Listen("tcp", cfg.MetricsAddr)
			if err != nil {
				return fmt.Errorf("metrics: cannot bind %s: %w", cfg.MetricsAddr, err)
			}
			go func() {
				if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
					logger.Error("servidor de metricas encerrou", slog.String("error", err.Error()))
				}
			}()
			logger.Info("servidor de metricas iniciado", slog.String("addr", cfg.MetricsAddr))
			return nil
		},
		OnStop: func(ctx context.Context) error { return server.Shutdown(ctx) },
	})
}

// NewRouter monta o roteador com as dependências resolvidas.
func NewRouter(
	handlers *httpapi.Handlers,
	verifier *keycloak.Verifier,
	ids ports.IDGenerator,
	logger *slog.Logger,
	collector *metrics.Metrics,
	checks []httpapi.HealthChecker,
) http.Handler {
	return httpapi.NewRouter(httpapi.RouterDeps{
		Handlers:     handlers,
		Verifier:     verifier,
		IDGenerator:  ids,
		Logger:       logger,
		Metrics:      collector,
		HealthChecks: checks,
	})
}

// NewHealthChecks agrupa as dependências verificáveis pelo readiness.
func NewHealthChecks(pool *pgxpool.Pool, sqsCheck *observability.SQSHealth) []httpapi.HealthChecker {
	return []httpapi.HealthChecker{
		observability.NewPostgresHealth(pool),
		sqsCheck,
	}
}

// StartHTTPServer sobe o servidor HTTP com lifecycle e shutdown gracioso. O net.Listen
// acontece dentro do OnStart e o erro é devolvido: sem isso, um ListenAndServe em
// goroutine falharia (porta ocupada) depois de o OnStart devolver sucesso, e a aplicação
// subiria "saudável" sem atender ninguém — falhar no bind é falhar na inicialização (§4).
func StartHTTPServer(lc fx.Lifecycle, router http.Handler, cfg config.Config, logger *slog.Logger) {
	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: router,
		// ReadHeaderTimeout protege contra conexões que enviam cabeçalhos eternamente
		// (Slowloris); os demais limitam leitura, escrita e ociosidade.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.HTTPReadTimeout,
		WriteTimeout:      cfg.HTTPWriteTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			// net.Listen explícito: um bind que falha interrompe a inicialização, em vez
			// de deixar o processo de pé sem atender ninguém.
			listener, err := net.Listen("tcp", cfg.HTTPAddr)
			if err != nil {
				return fmt.Errorf("http: cannot bind %s: %w", cfg.HTTPAddr, err)
			}

			go func() {
				if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
					logger.Error("servidor http encerrou inesperadamente", slog.String("error", err.Error()))
				}
			}()

			logger.Info("servidor http iniciado", slog.String("addr", cfg.HTTPAddr))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// Shutdown para de aceitar novas conexões e espera as em andamento (§4).
			logger.Info("encerrando servidor http")
			return server.Shutdown(ctx)
		},
	})
}
