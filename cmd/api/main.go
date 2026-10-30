package main

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditoutbox"
	"github.com/j-s-te/contract-management/internal/bootstrap"
	"github.com/j-s-te/contract-management/internal/config"
	"github.com/j-s-te/contract-management/internal/filegatewayclient"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/j-s-te/contract-management/internal/integration/crm"
	notificationintegration "github.com/j-s-te/contract-management/internal/integration/notification"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	settlementintegration "github.com/j-s-te/contract-management/internal/integration/settlement"
	"github.com/j-s-te/contract-management/internal/transport/httpapi"
	"github.com/oklog/ulid/v2"
)

// main 装配合同API及默认关闭的文件接入能力。
// 参数：无；使用受校验的进程运行配置。
// 返回值：无；初始化失败以非零状态退出。
func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	splitProcess, err := bootstrap.RuntimeProcessMode("contract-api")
	if err != nil {
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
	if err := platform.SyncAuthorizationCatalog(ctx, platform.CatalogSyncOptions{
		Enabled: cfg.PlatformCatalogSync, BaseURL: cfg.PlatformBaseURL,
		ApplicationID: cfg.PlatformApplicationID, ClientID: cfg.PlatformCatalogClientID,
		ClientSecret: cfg.PlatformCatalogSecret,
	}); err != nil {
		logger.Error("authorization catalog synchronization failed", "error", err)
		os.Exit(1)
	}
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
	repository := store.NewRepository(db)
	if !splitProcess {
		go (&crm.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.CRMDeliveryBaseURL, Token: cfg.CRMDeliveryToken, MaxAttempts: 20, Poll: 2 * time.Second}).Run(ctx)
	}
	personnelDirectory := platform.NewPersonnelDirectory(cfg.PlatformBaseURL, cfg.PlatformPersonnelClientID, cfg.PlatformPersonnelSecret, cfg.OIDCAuthorizationTimeout)
	// 事务内审计 outbox 的投递循环独立于通知配置：通知关闭、审计开启时仍必须消费，
	// 否则事务审计只落库不投递。
	auditReporter := platform.NewAuditReporter(cfg.PlatformBaseURL, cfg.PlatformAuditClientID, cfg.PlatformAuditClientSecret, cfg.PlatformApplicationCode, cfg.PlatformEnvironmentCode)
	var auditStore *auditoutbox.GORMStore
	if auditReporter != nil {
		auditStore = &auditoutbox.GORMStore{DB: db, Lease: 2 * time.Minute, MaxTries: 20}
		auditDispatcher := &auditoutbox.Dispatcher{
			Store:    auditStore,
			Reporter: auditReporter, WorkerID: "contract-api-audit", Poll: 2 * time.Second, BatchSize: 50,
			MaxAttempts: 20, MaxBackoff: 5 * time.Minute, Logger: logger,
		}
		go auditDispatcher.Run(ctx)
	}
	if cfg.PlatformNotificationClientID != "" && cfg.PlatformNotificationSecret != "" {
		notificationDispatcher := &notificationintegration.Dispatcher{
			Store: repository, BaseURL: cfg.PlatformBaseURL, MaxAttempts: 20, Poll: 2 * time.Second, Logger: logger,
			TokenSource: notificationintegration.NewClientCredentialsTokenSource(ctx, strings.TrimRight(cfg.PlatformBaseURL, "/")+"/oauth2/token", cfg.PlatformNotificationClientID, cfg.PlatformNotificationSecret),
			ResolveRoleRecipients: func(ctx context.Context, tenantID string, roleCodes []string) ([]string, error) {
				refs, err := personnelDirectory.ListEligibleUsers(ctx, application.Principal{TenantID: tenantID}, roleCodes)
				if err != nil {
					return nil, err
				}
				ids := make([]string, 0, len(refs))
				for _, ref := range refs {
					ids = append(ids, ref.UserID)
				}
				return ids, nil
			},
		}
		go notificationDispatcher.Run(ctx)
	}
	oidcStore, err := platform.NewGORMOIDCStore(db)
	if err != nil {
		logger.Error("OIDC session store failed", "error", err)
		os.Exit(1)
	}
	var projectDetectionCategories application.DetectionCategoryDirectory
	var crmContractReferences application.CRMContractReferenceDirectory
	if cfg.ProjectIntegrationEnabled {
		tokenSource := projectintegration.NewClientCredentialsTokenSource(ctx, cfg.ProjectIntegrationTokenURL, cfg.ProjectIntegrationClientID, cfg.ProjectIntegrationClientSecret, cfg.ProjectIntegrationAudience)
		dispatcher := &projectintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.ProjectAPIBaseURL, MaxAttempts: cfg.ProjectIntegrationRetries, Poll: cfg.ProjectIntegrationPoll, Logger: logger,
			TokenSource: tokenSource}
		if !splitProcess {
			go dispatcher.Run(ctx)
		}
		projectDetectionCategories = &projectintegration.DetectionCategoryDirectory{BaseURL: cfg.ProjectAPIBaseURL, TokenSource: tokenSource}
	}
	if cfg.SettlementIntegrationEnabled {
		settlementDispatcher := &settlementintegration.Dispatcher{Store: repository, LicenseGate: licenseGate, BaseURL: cfg.SettlementAPIBaseURL, MaxAttempts: cfg.SettlementIntegrationRetries, Poll: cfg.SettlementIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.SettlementIntegrationTokenURL, cfg.SettlementIntegrationClientID, cfg.SettlementIntegrationClientSecret, cfg.SettlementIntegrationAudience)}
		if !splitProcess {
			go settlementDispatcher.Run(ctx)
		}
	}
	if cfg.CRMReferenceEnabled {
		crmTokenSource := crm.NewClientCredentialsTokenSource(ctx, cfg.CRMReferenceTokenURL, cfg.CRMReferenceClientID, cfg.CRMReferenceClientSecret, cfg.CRMReferenceScope)
		crmContractReferences = &crm.ContractReferenceDirectory{BaseURL: cfg.CRMReferenceBaseURL, TokenSource: crmTokenSource}
	}
	service := &application.Service{
		ExternalPDFEnabled:              cfg.ExternalPDFEnabled,
		ContractImportEnabled:           cfg.ContractImportEnabled,
		ContractImportHistoryEnabled:    cfg.ContractImportHistoryEnabled,
		ContractImportConfirmationRoles: cfg.ContractImportConfirmationRoles,
		ContractImportMaxItems:          cfg.ContractImportMaxItems,
		ContractImportStagingTTL:        cfg.ContractImportStagingTTL,
		LicenseGate:                     licenseGate,
		Repo:                            repository,
		Templates:                       repository,
		Temporal:                        temporalClient,
		TaskQueue:                       cfg.TemporalTaskQueue,
		NodeTimeout:                     cfg.NodeTimeout,
		ReminderInterval:                cfg.ReminderInterval,
		Personnel:                       personnelDirectory,
		OpportunityLinkNotifier:         &crm.LinkNotifier{LicenseGate: licenseGate, BaseURL: cfg.CRMDeliveryBaseURL, Token: cfg.CRMDeliveryToken, Client: &http.Client{Timeout: 5 * time.Second}},
		DetectionCategories:             projectDetectionCategories,
		CRMContractReferences:           crmContractReferences,
	}
	if cfg.StampedFileMode != "legacy" {
		gatewayToken := filegatewayclient.NewClientCredentialsTokenSource(ctx, strings.TrimRight(cfg.PlatformBaseURL, "/")+"/oauth2/token", cfg.StampedFileClientID, cfg.StampedFileClientSecret, cfg.StampedFileScope)
		gateway, gatewayErr := filegatewayclient.New(cfg.StampedFileGatewayURL, &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}, gatewayToken)
		if gatewayErr != nil {
			logger.Error("file gateway initialization failed", "error", gatewayErr)
			os.Exit(1)
		}
		service.StampedFileGateway = gateway
		service.StampedFileMode = cfg.StampedFileMode
		service.StampedFileApplicationID = cfg.StampedFileApplicationID
		if !splitProcess {
			go (&application.TemplateFileDispatcher{Store: repository, Gateway: gateway, ApplicationID: cfg.StampedFileApplicationID, TenantID: cfg.OIDCTenantID, LicenseGate: licenseGate, Logger: logger}).Run(ctx)
		}
	}
	var dashboardBearer platform.ClientCredentialsTokenVerifier
	if cfg.DashboardMachineEnabled && cfg.DashboardMachineRequireBearer {
		dashboardBearer, err = platform.NewClientCredentialsTokenVerifier(ctx, platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.DashboardMachineIssuer, Audience: cfg.DashboardMachineAudience, PublicKeyPath: cfg.DashboardMachinePublicKeyPath,
			ClientID: cfg.DashboardMachineClientID, TenantID: cfg.OIDCTenantID,
			CallerApplicationCode: cfg.DashboardMachineCallerApp, CallerEnvironmentCode: cfg.DashboardMachineCallerEnv,
			RequiredScope: cfg.DashboardMachineScope,
		})
		if err != nil {
			logger.Error("initialize dashboard machine bearer verifier", "error", err)
			os.Exit(1)
		}
	}
	var settlementBearer platform.ClientCredentialsTokenVerifier
	if cfg.SettlementMachineEnabled && cfg.SettlementMachineRequireBearer {
		settlementBearer, err = platform.NewKeycloakClientCredentialsTokenVerifier(ctx, platform.KeycloakClientCredentialsVerifierOptions{
			Issuer: cfg.OIDCIssuer, BackchannelBaseURL: cfg.OIDCBackchannelBaseURL,
			ClientID: cfg.SettlementMachineClientID, Audience: cfg.SettlementMachineAudience,
			TenantID: cfg.OIDCTenantID, Timeout: cfg.OIDCAuthorizationTimeout,
		})
		if err != nil {
			logger.Error("initialize settlement machine bearer verifier", "error", err)
			os.Exit(1)
		}
	}
	// 写路径冻结覆盖与协议端点共用同一仓储：即使端点未启用，也不得把新责任分配给交接中的用户。
	service.HandoverFreeze = repository
	// 审批节点转派走权威工作流命令（与 HTTP transfer 动作同一信号通道）。
	var handoverBearer platform.ClientCredentialsTokenVerifier
	if cfg.HandoverMachineEnabled && cfg.HandoverMachineRequireBearer {
		handoverBearer, err = platform.NewKeycloakClientCredentialsTokenVerifier(ctx, platform.KeycloakClientCredentialsVerifierOptions{
			Issuer: cfg.OIDCIssuer, BackchannelBaseURL: cfg.OIDCBackchannelBaseURL,
			ClientID: cfg.HandoverMachineClientID, Audience: cfg.HandoverMachineAudience,
			TenantID: cfg.OIDCTenantID, Timeout: cfg.OIDCAuthorizationTimeout,
		})
		if err != nil {
			logger.Error("initialize handover machine bearer verifier", "error", err)
			os.Exit(1)
		}
	}
	var projectApprovalBearer platform.ClientCredentialsTokenVerifier
	if cfg.ProjectApprovalMachineEnabled {
		projectApprovalBearer, err = platform.NewClientCredentialsTokenVerifier(ctx, platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.ProjectApprovalMachineIssuer, Audience: cfg.ProjectApprovalMachineAudience,
			PublicKeyPath: cfg.ProjectApprovalMachinePublicKeyPath, ClientID: cfg.ProjectApprovalMachineClientID,
			TenantID: cfg.OIDCTenantID, CallerApplicationCode: cfg.ProjectApprovalMachineCallerApp,
			CallerEnvironmentCode: cfg.ProjectApprovalMachineCallerEnv, RequiredScope: cfg.ProjectApprovalMachineScope,
		})
		if err != nil {
			logger.Error("initialize project approval bearer verifier", "error", err)
			os.Exit(1)
		}
	}
	var crmOpportunityIntakeBearer platform.ClientCredentialsTokenVerifier
	if cfg.CRMOpportunityIntakeMachineEnabled {
		crmOpportunityIntakeBearer, err = platform.NewClientCredentialsTokenVerifier(ctx, platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.CRMOpportunityIntakeMachineIssuer, Audience: cfg.CRMOpportunityIntakeMachineAudience,
			PublicKeyPath: cfg.CRMOpportunityIntakeMachinePublicKeyPath, ClientID: cfg.CRMOpportunityIntakeMachineClientID,
			TenantID: cfg.OIDCTenantID, CallerApplicationCode: cfg.CRMOpportunityIntakeMachineCallerApp,
			CallerEnvironmentCode: cfg.CRMOpportunityIntakeMachineCallerEnv, RequiredScope: cfg.CRMOpportunityIntakeMachineScope,
		})
		if err != nil {
			logger.Error("initialize CRM opportunity intake bearer verifier", "error", err)
			os.Exit(1)
		}
	}
	var crmSignedCountBearer platform.ClientCredentialsTokenVerifier
	if cfg.CRMSignedCountMachineEnabled {
		crmSignedCountBearer, err = platform.NewClientCredentialsTokenVerifier(ctx, platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.CRMSignedCountMachineIssuer, Audience: cfg.CRMSignedCountMachineAudience,
			PublicKeyPath: cfg.CRMSignedCountMachinePublicKeyPath, ClientID: cfg.CRMSignedCountMachineClientID,
			TenantID: cfg.OIDCTenantID, CallerApplicationCode: cfg.CRMSignedCountMachineCallerApp,
			CallerEnvironmentCode: cfg.CRMSignedCountMachineCallerEnv, RequiredScope: cfg.CRMSignedCountMachineScope,
		})
		if err != nil {
			logger.Error("initialize CRM signed contract count bearer verifier", "error", err)
			os.Exit(1)
		}
	}
	identity, err := platform.NewOIDCAuthenticator(ctx, platform.OIDCOptions{
		Issuer: cfg.OIDCIssuer, BackchannelBaseURL: cfg.OIDCBackchannelBaseURL,
		ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret,
		RedirectURI: cfg.OIDCRedirectURI, PostLogoutRedirectURI: cfg.OIDCPostLogoutRedirectURI,
		IdentityProviderHint: cfg.OIDCIDPHint,
		Scopes:               cfg.OIDCScopes, TenantID: cfg.OIDCTenantID, SessionCookieName: cfg.OIDCSessionCookieName,
		SessionTTL: cfg.OIDCSessionTTL, SessionSecure: cfg.OIDCSessionSecure,
		AuthorizationRefreshInterval: cfg.OIDCAuthorizationRefresh,
		AuthorizationTimeout:         cfg.OIDCAuthorizationTimeout,
		AuthorizationMaxStale:        cfg.OIDCAuthorizationMaxStale,
		PlatformBaseURL:              cfg.PlatformBaseURL,
		ApplicationCode:              cfg.PlatformApplicationCode,
		EnvironmentCode:              cfg.PlatformEnvironmentCode,
		SessionEncryptionKey:         cfg.OIDCSessionEncryptionKey,
		Store:                        oidcStore,
		PathPrefix:                   cfg.AppPathPrefix,
		Audit:                        auditReporter,
	})
	if err != nil {
		logger.Error("OIDC discovery failed", "error", err)
		os.Exit(1)
	}
	// 人员交接机器协议：仅在启用时注册端点；未启用时不暴露该组路由。
	if resolver, ok := personnelDirectory.(application.HandoverOwnerResolver); ok {
		service.ImportOwners = resolver
	}
	for _, entry := range []struct {
		cfg config.ImportTargetConfig
		out **application.ImportTargetTransport
	}{{cfg.ImportProject, &service.ImportProject}, {cfg.ImportFinance, &service.ImportFinance}, {cfg.ImportReport, &service.ImportReport}} {
		if entry.cfg.BaseURL != "" {
			*entry.out = &application.ImportTargetTransport{BaseURL: entry.cfg.BaseURL, TokenSource: filegatewayclient.NewClientCredentialsTokenSource(ctx, entry.cfg.TokenURL, entry.cfg.ClientID, entry.cfg.ClientSecret, entry.cfg.Scope)}
		}
	}
	service.ImportActorResolver = identity.ResolveImportActor
	go service.RunImportDispatcher(ctx)
	service.ImportReportReadConfigured = cfg.ImportReportRead.ClientID != ""
	var importReportVerifier platform.ClientCredentialsTokenVerifier
	if cfg.ImportReportRead.ClientID != "" {
		importReportVerifier, err = platform.NewClientCredentialsTokenVerifier(ctx, platform.ClientCredentialsVerifierOptions{Issuer: cfg.ImportReportRead.Issuer, Audience: cfg.ImportReportRead.Audience, PublicKeyPath: cfg.ImportReportRead.PublicKeyPath, ClientID: cfg.ImportReportRead.ClientID, TenantID: cfg.OIDCTenantID, CallerApplicationCode: "data_analysis", CallerEnvironmentCode: cfg.ImportReportRead.CallerEnvironmentCode, RequiredScope: "contract.import.report.read"})
		if err != nil {
			logger.Error("initialize import report verifier", "error", err)
			os.Exit(1)
		}
	}

	var handoverService *application.HandoverService
	if cfg.HandoverMachineEnabled {
		handoverService, err = application.NewHandoverService(store.NewRepository(db),
			func(time.Time) (string, error) { return ulid.Make().String(), nil }, time.Now)
		if err != nil {
			logger.Error("initialize handover service", "error", err)
			os.Exit(1)
		}
		// 审批节点转派走权威工作流命令（与 HTTP transfer 动作同一信号通道）。
		handoverService.SetApprovalTransferDispatcher(application.NewHandoverApprovalDispatcher(service))
		// 承接人的姓名/身份来自平台权威负责人目录，不能只改 owner_user_id。
		if resolver, ok := personnelDirectory.(application.HandoverOwnerResolver); ok {
			handoverService.SetOwnerResolver(resolver)
		}
	}
	// 统一审计来源：显式可信代理列表，未列入的直接对端不能改写来源地址。
	httpapi.ConfigureTrustedProxies(cfg.TrustedProxies)
	server := &http.Server{
		Addr: cfg.HTTPAddress,
		Handler: httpapi.NewRouterWithAuditOutbox(service, identity,
			&httpapi.DashboardIntegrationOptions{Enabled: cfg.DashboardMachineEnabled, RequireBearer: cfg.DashboardMachineRequireBearer, BearerVerifier: dashboardBearer},
			&httpapi.SettlementIntegrationOptions{Enabled: cfg.SettlementMachineEnabled, RequireBearer: cfg.SettlementMachineRequireBearer, BearerVerifier: settlementBearer},
			&httpapi.ProjectIntegrationOptions{Enabled: cfg.ProjectApprovalMachineEnabled, RequireBearer: cfg.ProjectApprovalMachineRequireBearer, BearerVerifier: projectApprovalBearer},
			&httpapi.CRMSignedCountIntegrationOptions{Enabled: cfg.CRMSignedCountMachineEnabled, RequireBearer: cfg.CRMSignedCountMachineRequireBearer, BearerVerifier: crmSignedCountBearer, OpportunityIntakeEnabled: cfg.CRMOpportunityIntakeMachineEnabled, OpportunityIntakeRequireBearer: cfg.CRMOpportunityIntakeMachineRequireBearer, OpportunityIntakeBearerVerifier: crmOpportunityIntakeBearer}, handoverService, &httpapi.HandoverIntegrationOptions{Enabled: cfg.HandoverMachineEnabled, RequireBearer: cfg.HandoverMachineRequireBearer, BearerVerifier: handoverBearer}, auditStore, auditReporter),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}

	httpapi.RegisterImportReportSource(server.Handler.(*gin.Engine), service, importReportVerifier)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	logger.Info("contract API started", "address", cfg.HTTPAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}
