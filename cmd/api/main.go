// Entrypoint da API: apenas ciclo de vida (construir, iniciar, esperar sinal, parar).
// Start/Stop explícitos deixam o prazo de encerramento vindo da configuração, em vez do
// valor fixo de app.Run().
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

// startTimeout: prazo para a aplicação subir (validar config, conectar, checar).
const startTimeout = 30 * time.Second

func main() {
	// Logger do main, usado antes de o logger configurado existir.
	boot := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(slog.String("service", "wager-backend"))

	// Config validada ANTES de montar o grafo: se falta variável, a mensagem sai
	// imediata com a lista do que falta — mais acionável que um panic do Fx.
	cfg, err := config.Load()
	if err != nil {
		boot.Error("configuracao invalida", slog.String("error", err.Error()))
		os.Exit(1)
	}

	app := bootstrap.NewAPIApp(cfg)

	startCtx, cancelStart := context.WithTimeout(context.Background(), startTimeout)
	defer cancelStart()

	// Erros aqui são de operador (config, banco inacessível). Wiring quebrado já é panic
	// em fx.New — erro de desenvolvedor, que deve aparecer de imediato.
	if err := app.Start(startCtx); err != nil {
		boot.Error("falha ao iniciar a aplicacao", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Canal explícito deixa claro quais sinais são tratados: SIGINT e SIGTERM.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	received := <-signals
	boot.Info("sinal recebido; iniciando encerramento gracioso", slog.String("signal", received.String()))

	// Prazo de encerramento da mesma configuração validada no início.
	stopCtx, cancelStop := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelStop()

	if err := app.Stop(stopCtx); err != nil {
		boot.Error("erro no encerramento", slog.String("error", err.Error()))
		os.Exit(1)
	}

	boot.Info("aplicacao encerrada")
}
