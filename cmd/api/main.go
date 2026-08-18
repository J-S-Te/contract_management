package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/bootstrap"
	"github.com/j-s-te/contract-management/internal/config"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/j-s-te/contract-management/internal/integration/crm"
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
		Personnel:               platform.NewPersonnelDirectory(cfg.PlatformBaseURL, cfg.PlatformPersonnelClientID, cfg.PlatformPersonnelSecret, cfg.OIDCAuthorizationTimeout),
		OpportunityLinkNotifier: &crm.LinkNotifier{BaseURL: os.Getenv("CRM_API_BASE_URL"), Token: os.Getenv("CRM_API_TOKEN"), Client: &http.Client{Timeout: 5 * time.Second}},
	}
	var dashboardBearer platform.ClientCredentialsTokenVerifier
	if cfg.DashboardMachineEnabled && cfg.DashboardMachineRequireBearer {
		dashboardBearer, err = platform.NewClientCredentialsTokenVerifier(ctx, platform.ClientCredentialsVerifierOptions{
			Issuer: cfg.OIDCIssuer, BackchannelBaseURL: cfg.OIDCBackchannelBaseURL,
			ClientID: cfg.DashboardMachineClientID, Audience: cfg.DashboardMachineAudience,
			Timeout: cfg.OIDCAuthorizationTimeout,
		})
		if err != nil {
			logger.Error("initialize dashboard machine bearer verifier", "error", err)
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
	})
	if err != nil {
		logger.Error("OIDC discovery failed", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              cfg.HTTPAddress,
		Handler:           httpapi.NewRouter(service, identity, &httpapi.DashboardIntegrationOptions{Enabled: cfg.DashboardMachineEnabled, RequireBearer: cfg.DashboardMachineRequireBearer, BearerVerifier: dashboardBearer}, platform.NewAuditReporter(cfg.PlatformBaseURL, cfg.PlatformAuditClientID, cfg.PlatformAuditClientSecret, cfg.PlatformApplicationCode, cfg.PlatformEnvironmentCode)),
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
