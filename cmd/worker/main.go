package main

import (
	"context"
	"errors"
	core "github.com/J-S-Te/license-core"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/j-s-te/contract-management/internal/bootstrap"
	"github.com/j-s-te/contract-management/internal/config"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/integration/crm"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	"github.com/j-s-te/contract-management/internal/temporalworker"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if _, err := bootstrap.RuntimeProcessMode("contract-worker"); err != nil {
		logger.Error("contract process configuration failed", "error", err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	licenseGate, err := bootstrap.StartCommercialLicense(ctx, logger)
	if err != nil {
		logger.Error("commercial license configuration failed")
		os.Exit(1)
	}
	db, err := bootstrap.OpenDatabase(ctx, cfg.MySQLDSN)
	if err != nil {
		logger.Error("database failed", "error", err)
		os.Exit(1)
	}
	defer bootstrap.CloseDatabase(db)
	metrics := temporalworker.NewMetricsRegistry()
	var ready atomic.Bool
	if err := temporalworker.StartMetricsServerWithReadiness(ctx, cfg.TemporalMetricsAddress, metrics, logger, ready.Load); err != nil {
		logger.Error("temporal metrics configuration failed", "error", err)
		os.Exit(1)
	}
	temporalClient, err := bootstrap.OpenTemporal(ctx, cfg, metrics)
	if err != nil {
		logger.Error("temporal failed", "error", err)
		os.Exit(1)
	}
	defer temporalClient.Close()
	workerOptions, err := temporalworker.WorkerOptions(temporalworker.VersioningConfig{
		Enabled: cfg.TemporalWorkerVersioning, DeploymentName: cfg.TemporalWorkerDeploymentName,
		BuildID: cfg.TemporalWorkerBuildID, Policy: cfg.TemporalWorkerVersioningPolicy,
	})
	if err != nil {
		logger.Error("temporal worker versioning configuration failed", "error", err)
		os.Exit(1)
	}
	workerOptions.WorkerStopTimeout = 30 * time.Second
	// Worker Deployment Versioning 控制任务路由；工作流内部的破坏性分支仍必须使用
	// workflow.GetVersion，二者不能互相替代。
	w := worker.New(temporalClient, cfg.TemporalTaskQueue, workerOptions)
	repository := store.NewRepository(db)
	go (&crm.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.CRMDeliveryBaseURL, Token: cfg.CRMDeliveryToken, MaxAttempts: 20, Poll: 2 * time.Second}).Run(ctx)
	workflows.Register(w, &workflows.Activities{Store: repository, LicenseGate: licenseGate})
	if cfg.ProjectIntegrationEnabled {
		dispatcher := &projectintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.ProjectAPIBaseURL, MaxAttempts: cfg.ProjectIntegrationRetries, Poll: cfg.ProjectIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.ProjectIntegrationTokenURL, cfg.ProjectIntegrationClientID, cfg.ProjectIntegrationClientSecret, cfg.ProjectIntegrationAudience)}
		go dispatcher.Run(ctx)
	}
	// Keep polling alive for recovery; expiry pauses new scheduling and every
	// activity rechecks entitlement, including workflows created before expiry.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			if licenseGate.Check(ctx, core.MUTATE_BUSINESS) == nil {
				_, scheduleErr := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "contract-auto-archive-daily", TaskQueue: cfg.TemporalTaskQueue, CronSchedule: cfg.ArchiveCron}, workflows.ExpiredArchiveWorkflowName, workflows.ExpiredArchiveInput{})
				var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
				if scheduleErr != nil && !errors.As(scheduleErr, &alreadyStarted) && ctx.Err() == nil {
					logger.Error("start archive cron workflow failed", "error", scheduleErr)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	logger.Info("contract workflow worker started", "task_queue", cfg.TemporalTaskQueue,
		"deployment", cfg.TemporalWorkerDeploymentName, "build_id", cfg.TemporalWorkerBuildID,
		"versioning_enabled", cfg.TemporalWorkerVersioning, "versioning_policy", cfg.TemporalWorkerVersioningPolicy)
	if err := w.Start(); err != nil {
		logger.Error("worker failed", "error", err)
		os.Exit(1)
	}
	ready.Store(true)
	<-ctx.Done()
	ready.Store(false)
	w.Stop()
}
