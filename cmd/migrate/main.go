package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		logger.Error("MYSQL_DSN is required")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := migration.Run(ctx, dsn, migrations.Files); err != nil {
		logger.Error("contract migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("contract migrations completed")
}
