// Command picapi-server runs the picapi HTTP server.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"picapi/internal/server"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		logger.Error("failed to load .env", "error", err)
		os.Exit(1)
	}
	cfg, err := server.LoadConfig()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
