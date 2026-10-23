package main

import (
	"context"
	"errors"
	core "github.com/J-S-Te/license-core"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/bootstrap"
	"github.com/j-s-te/contract-management/internal/config"
	"github.com/j-s-te/contract-management/internal/filegatewayclient"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/integration/crm"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	settlementintegration "github.com/j-s-te/contract-management/internal/integration/settlement"
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
	if cfg.StampedFileMode != "legacy" {
		token := filegatewayclient.NewClientCredentialsTokenSource(ctx, strings.TrimRight(cfg.PlatformBaseURL, "/")+"/oauth2/token", cfg.StampedFileClientID, cfg.StampedFileClientSecret, cfg.StampedFileScope)
		gateway, err := filegatewayclient.New(cfg.StampedFileGatewayURL, &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, token)
		if err != nil {
			logger.Error("file gateway initialization failed", "error", err)
			os.Exit(1)
		}
		go (&application.TemplateFileDispatcher{Store: repository, Gateway: gateway, ApplicationID: cfg.StampedFileApplicationID, TenantID: cfg.OIDCTenantID, LicenseGate: licenseGate, Logger: logger}).Run(ctx)
	}
	go (&crm.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.CRMDeliveryBaseURL, Token: cfg.CRMDeliveryToken, MaxAttempts: 20, Poll: 2 * time.Second}).Run(ctx)
	workflows.Register(w, &workflows.Activities{Store: repository, LicenseGate: licenseGate})
	if cfg.ProjectIntegrationEnabled {
		dispatcher := &projectintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.ProjectAPIBaseURL, MaxAttempts: cfg.ProjectIntegrationRetries, Poll: cfg.ProjectIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.ProjectIntegrationTokenURL, cfg.ProjectIntegrationClientID, cfg.ProjectIntegrationClientSecret, cfg.ProjectIntegrationAudience)}
		go dispatcher.Run(ctx)
	}
	// 合同 → 结算 财务生效事件投递。与项目投递相互独立：项目投递可因来源校验未就绪而停用，
	// 财务事件仍须按结算侧要求的机器令牌投递。
	if cfg.SettlementIntegrationEnabled {
		settlementDispatcher := &settlementintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.SettlementAPIBaseURL, MaxAttempts: cfg.SettlementIntegrationRetries, Poll: cfg.SettlementIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.SettlementIntegrationTokenURL, cfg.SettlementIntegrationClientID, cfg.SettlementIntegrationClientSecret, cfg.SettlementIntegrationAudience)}
		go settlementDispatcher.Run(ctx)
	}
	// 保持轮询以支持恢复；过期会暂停新调度，且每个
	// 活动均重检权益，包括过期前创建的工作流。
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
