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
	"github.com/j-s-te/contract-management/internal/lifecycle"
	"github.com/j-s-te/contract-management/internal/temporalworker"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// main 装配合同工作流及明确启用的出站消费者。
// 参数：无，读取当前进程配置。
// 返回值：无；配置或初始化失败以非零状态退出。
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
	// 退出预算：默认 20s，可用 CONTRACT_SHUTDOWN_DRAIN_SECONDS（秒）覆盖；
	// 非法值或 <=0 回落默认，避免配置手误把进程退出变成立即杀后台或无限等待。
	shutdownBudget := lifecycle.DrainBudget(os.Getenv("CONTRACT_SHUTDOWN_DRAIN_SECONDS"), lifecycle.DefaultDrainBudget)
	background := lifecycle.NewWaiter(shutdownBudget)
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
		templateFileDispatcher := &application.TemplateFileDispatcher{Store: repository, Gateway: gateway, ApplicationID: cfg.StampedFileApplicationID, TenantID: cfg.OIDCTenantID, LicenseGate: licenseGate, Logger: logger}
		background.Go("template-file-outbox", func() { templateFileDispatcher.Run(ctx) })
	}
	// 合同→CRM 关联回写与 API 进程使用同一平台 client_credentials 专用用途令牌；
	// 有效开关关闭时不构造令牌来源或回写投递器。
	if cfg.CRMLinkEnabled {
		crmLinkDispatcher := &crm.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.CRMLinkBaseURL, MaxAttempts: 20, Poll: 2 * time.Second, Logger: logger,
			TokenSource: crm.NewClientCredentialsTokenSource(ctx, cfg.CRMLinkTokenURL, cfg.CRMLinkClientID, cfg.CRMLinkClientSecret, cfg.CRMLinkScope)}
		background.Go("crm-link-outbox", func() { crmLinkDispatcher.Run(ctx) })
	}
	workflows.Register(w, &workflows.Activities{Store: repository, LicenseGate: licenseGate,
		NumberAuditor: repository, NumberAuditMetrics: metrics, Logger: logger})
	if cfg.ProjectIntegrationEnabled {
		dispatcher := &projectintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.ProjectAPIBaseURL, MaxAttempts: cfg.ProjectIntegrationRetries, Poll: cfg.ProjectIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.ProjectIntegrationTokenURL, cfg.ProjectIntegrationClientID, cfg.ProjectIntegrationClientSecret, cfg.ProjectIntegrationAudience)}
		background.Go("project-outbox", func() { dispatcher.Run(ctx) })
	}
	// 合同 → 结算 财务生效事件投递。与项目投递相互独立：项目投递可因来源校验未就绪而停用，
	// 财务事件仍须按结算侧要求的机器令牌投递。
	if cfg.SettlementIntegrationEnabled {
		settlementDispatcher := &settlementintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.SettlementAPIBaseURL, MaxAttempts: cfg.SettlementIntegrationRetries, Poll: cfg.SettlementIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.SettlementIntegrationTokenURL, cfg.SettlementIntegrationClientID, cfg.SettlementIntegrationClientSecret, cfg.SettlementIntegrationAudience)}
		background.Go("settlement-outbox", func() { settlementDispatcher.Run(ctx) })
	}
	// 保持轮询以支持恢复；过期会暂停新调度，且每个
	// 活动均重检权益，包括过期前创建的工作流。
	background.Go("archive-cron", func() {
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
	})
	// 合同编号登记表漂移的每日只读巡检：沿用归档 cron 的同一注册范式（30s ticker +
	// 固定 workflow ID + CronSchedule），漂移只记日志/指标，不主动通知任何人。
	// NUMBER_AUDIT_CRON_SCHEDULE 显式配成空串表示禁用：此时不注册该 cron。不能把空串
	// 直接传给 CronSchedule——Temporal 会把空 cron 当成“只跑一次”而不是“不跑”。
	// 巡检只读，因此刻意不套商业许可证写权限门禁（许可过期时仍需要看到数据漂移）。
	if cfg.NumberAuditCron != "" {
		background.Go("number-audit-cron", func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				_, scheduleErr := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "contract-number-audit-daily", TaskQueue: cfg.TemporalTaskQueue, CronSchedule: cfg.NumberAuditCron}, workflows.ContractNumberAuditWorkflowName)
				var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
				if scheduleErr != nil && !errors.As(scheduleErr, &alreadyStarted) && ctx.Err() == nil {
					logger.Error("start contract number audit cron workflow failed", "error", scheduleErr)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		})
	} else {
		logger.Info("contract number audit cron disabled", "reason", "NUMBER_AUDIT_CRON_SCHEDULE 为空")
	}
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
	// Temporal Stop 顺序保持现状：先停 worker 再 join 后台投递 goroutine。
	w.Stop()
	// 投递 goroutine 随 ctx 取消返回；在途批次用独立 ctx 完成落库。
	// 超预算列出未退出组件后以非零码退出，不 panic：与 platform/Settlement 一致，
	// 后台组件没停下时不能以 0 退出，否则编排侧会把“排空超预算”误判成正常停机。
	if !background.LogUnfinished(logger, "contract workflow worker", shutdownBudget) {
		os.Exit(1)
	}
}
