package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/bootstrap"
	"github.com/j-s-te/contract-management/internal/config"
	"github.com/j-s-te/contract-management/internal/filegatewayclient"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/j-s-te/contract-management/internal/integration/crm"
	notificationintegration "github.com/j-s-te/contract-management/internal/integration/notification"
	projectintegration "github.com/j-s-te/contract-management/internal/integration/project"
	"github.com/j-s-te/contract-management/internal/transport/httpapi"
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
	go (&crm.Dispatcher{Store: repository, BaseURL: os.Getenv("CRM_API_BASE_URL"), Token: os.Getenv("CRM_API_TOKEN"), MaxAttempts: 20, Poll: 2 * time.Second}).Run(ctx)
	personnelDirectory := platform.NewPersonnelDirectory(cfg.PlatformBaseURL, cfg.PlatformPersonnelClientID, cfg.PlatformPersonnelSecret, cfg.OIDCAuthorizationTimeout)
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
	if cfg.ProjectIntegrationEnabled {
		dispatcher := &projectintegration.Dispatcher{Store: repository, BaseURL: cfg.ProjectAPIBaseURL, MaxAttempts: cfg.ProjectIntegrationRetries, Poll: cfg.ProjectIntegrationPoll, Logger: logger,
			TokenSource: projectintegration.NewClientCredentialsTokenSource(ctx, cfg.ProjectIntegrationTokenURL, cfg.ProjectIntegrationClientID, cfg.ProjectIntegrationClientSecret, cfg.ProjectIntegrationAudience)}
		go dispatcher.Run(ctx)
	}
	service := &application.Service{
		Repo:                    repository,
		Templates:               repository,
		Temporal:                temporalClient,
		TaskQueue:               cfg.TemporalTaskQueue,
		NodeTimeout:             cfg.NodeTimeout,
		ReminderInterval:        cfg.ReminderInterval,
		Personnel:               personnelDirectory,
		OpportunityLinkNotifier: &crm.LinkNotifier{BaseURL: os.Getenv("CRM_API_BASE_URL"), Token: os.Getenv("CRM_API_TOKEN"), Client: &http.Client{Timeout: 5 * time.Second}},
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
	auditReporter := platform.NewAuditReporter(cfg.PlatformBaseURL, cfg.PlatformAuditClientID, cfg.PlatformAuditClientSecret, cfg.PlatformApplicationCode, cfg.PlatformEnvironmentCode)
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
	server := &http.Server{
		Addr: cfg.HTTPAddress,
		Handler: httpapi.NewRouterWithSettlement(service, identity,
			&httpapi.DashboardIntegrationOptions{Enabled: cfg.DashboardMachineEnabled, RequireBearer: cfg.DashboardMachineRequireBearer, BearerVerifier: dashboardBearer},
			&httpapi.SettlementIntegrationOptions{Enabled: cfg.SettlementMachineEnabled, RequireBearer: cfg.SettlementMachineRequireBearer, BearerVerifier: settlementBearer}, auditReporter),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}

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
