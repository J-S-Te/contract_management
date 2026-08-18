package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/j-s-te/contract-management/internal/bootstrap"
	"github.com/j-s-te/contract-management/internal/config"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/integration/crm"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	db, err := bootstrap.OpenDatabase(ctx, cfg.MySQLDSN)
	if err != nil {
		logger.Error("database failed", "error", err)
		os.Exit(1)
	}
	defer bootstrap.CloseDatabase(db)
	temporalClient, err := bootstrap.OpenTemporal(ctx, cfg)
	if err != nil {
		logger.Error("temporal failed", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	w := worker.New(temporalClient, cfg.TemporalTaskQueue, worker.Options{DisableRegistrationAliasing: true})
	repository := store.NewRepository(db)
	go (&crm.Dispatcher{Store: repository, BaseURL: os.Getenv("CRM_API_BASE_URL"), Token: os.Getenv("CRM_API_TOKEN"), MaxAttempts: 20, Poll: 2 * time.Second}).Run(ctx)
	workflows.Register(w, &workflows.Activities{Store: repository})
	if cfg.ProjectIntegrationEnabled {
		dispatcher := &projectintegration.Dispatcher{Store: repository, BaseURL: cfg.ProjectAPIBaseURL, MaxAttempts: cfg.ProjectIntegrationRetries, Poll: cfg.ProjectIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.ProjectIntegrationTokenURL, cfg.ProjectIntegrationClientID, cfg.ProjectIntegrationClientSecret, cfg.ProjectIntegrationAudience)}
		go dispatcher.Run(ctx)
	}
	_, err = temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "contract-auto-archive-daily", TaskQueue: cfg.TemporalTaskQueue, CronSchedule: cfg.ArchiveCron}, workflows.ExpiredArchiveWorkflowName, workflows.ExpiredArchiveInput{})
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if err != nil && !errors.As(err, &alreadyStarted) {
		logger.Error("start archive cron workflow failed", "error", err)
		os.Exit(1)
	}
	logger.Info("contract workflow worker started", "task_queue", cfg.TemporalTaskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		logger.Error("worker failed", "error", err)
		os.Exit(1)
	}
}
