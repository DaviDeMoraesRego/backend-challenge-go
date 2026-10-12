// Entrypoint do worker (SQS, outbox, referências pendentes): apenas ciclo de vida.
// Binário separado da API — ver internal/bootstrap/apps.go (§8).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidemoraesrego/backend-challenge-go/internal/bootstrap"
	"github.com/davidemoraesrego/backend-challenge-go/internal/platform/config"
)

const startTimeout = 30 * time.Second

func main() {
	boot := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(slog.String("service", "wager-worker"))

	cfg, err := config.Load()
	if err != nil {
		boot.Error("configuracao invalida", slog.String("error", err.Error()))
		os.Exit(1)
	}

	app := bootstrap.NewWorkerApp(cfg)

	startCtx, cancelStart := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		boot.Error("falha ao iniciar o worker", slog.String("error", err.Error()))
		os.Exit(1)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	received := <-signals
	boot.Info("sinal recebido; encerrando workers",
		slog.String("signal", received.String()),
	)

	// O prazo de encerramento deve ser compatível com o visibility timeout da fila:
	// morto antes de concluir, o worker devolve a mensagem à fila (at-least-once) e gera
	// reprocessamento.
	stopCtx, cancelStop := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelStop()

	if err := app.Stop(stopCtx); err != nil {
		boot.Error("erro no encerramento dos workers", slog.String("error", err.Error()))
		os.Exit(1)
	}

	boot.Info("worker encerrado")
}
