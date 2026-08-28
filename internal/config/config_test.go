package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func validEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("MYSQL_DSN", "user:password@tcp(localhost:3306)/contracts?parseTime=true")
	t.Setenv("HTTP_ADDRESS", ":8081")
	t.Setenv("PLATFORM_BASE_URL", "http://localhost:8080")
	t.Setenv("OIDC_ISSUER", "http://localhost:8080")
	t.Setenv("OIDC_BACKCHANNEL_BASE_URL", "")
	t.Setenv("OIDC_CLIENT_ID", "contract_management-dev-web")
	t.Setenv("OIDC_CLIENT_SECRET", "test-client-secret")
	t.Setenv("OIDC_REDIRECT_URI", "http://localhost:8081/contract_management/auth/callback")
	t.Setenv("OIDC_IDP_HINT", "basic-platform")
	t.Setenv("OIDC_TENANT_ID", "01J00000000000000000000000")
	t.Setenv("OIDC_SESSION_COOKIE_NAME", "contract_management_session")
	t.Setenv("OIDC_SESSION_COOKIE_SECURE", "false")
	t.Setenv("OIDC_SESSION_TTL", "8h")
	t.Setenv("OIDC_AUTHORIZATION_REFRESH_INTERVAL", "1m")
	t.Setenv("OIDC_AUTHORIZATION_CONTEXT_TIMEOUT", "10s")
	t.Setenv("OIDC_AUTHORIZATION_MAX_STALE", "2m")
	t.Setenv("OIDC_SESSION_ENCRYPTION_KEY_BASE64", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	t.Setenv("APP_PATH_PREFIX", "/contract_management")
	t.Setenv("TEMPORAL_ADDRESS", "localhost:7233")
	t.Setenv("TEMPORAL_NAMESPACE", "default")
	t.Setenv("TEMPORAL_TASK_QUEUE", "contract-management")
	t.Setenv("TEMPORAL_TLS", "false")
	t.Setenv("TEMPORAL_WORKER_VERSIONING_ENABLED", "true")
	t.Setenv("TEMPORAL_WORKER_DEPLOYMENT_NAME", "contract-management")
	t.Setenv("TEMPORAL_WORKER_BUILD_ID", "contract-test-v1")
	t.Setenv("TEMPORAL_WORKER_VERSIONING_POLICY", "PINNED")
	t.Setenv("TEMPORAL_METRICS_ADDRESS", ":9091")
	t.Setenv("APPROVAL_NODE_TIMEOUT", "72h")
	t.Setenv("APPROVAL_REMINDER_INTERVAL", "24h")
	t.Setenv("PLATFORM_AUDIT_CLIENT_ID", "")
	t.Setenv("PLATFORM_AUDIT_CLIENT_SECRET", "")
	t.Setenv("PLATFORM_APPLICATION_CODE", "contract_management")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")
	t.Setenv("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED", "false")
	t.Setenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID", "")
	t.Setenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID", "")
	t.Setenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET", "")
}

func TestLoadRejectsIncompleteCatalogSynchronizationConfiguration(t *testing.T) {
	validEnvironment(t)
	t.Setenv("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED", "true")
	t.Setenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID", "app-1")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "catalog synchronization") {
		t.Fatalf("Load() error = %v, want incomplete catalog synchronization error", err)
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	validEnvironment(t)
	t.Setenv("APPROVAL_NODE_TIMEOUT", "tomorrow")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "APPROVAL_NODE_TIMEOUT") {
		t.Fatalf("Load() error = %v, want invalid duration error", err)
	}
}

func TestLoadRejectsInvalidTemporalTLS(t *testing.T) {
	validEnvironment(t)
	t.Setenv("TEMPORAL_TLS", "sometimes")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_TLS") {
		t.Fatalf("Load() error = %v, want invalid boolean error", err)
	}
}

func TestLoadRejectsInvalidTemporalWorkerVersioningPolicy(t *testing.T) {
	validEnvironment(t)
	t.Setenv("TEMPORAL_WORKER_VERSIONING_POLICY", "LATEST")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_WORKER_VERSIONING_POLICY") {
		t.Fatalf("Load() error = %v, want invalid worker versioning policy", err)
	}
}

func TestLoadRequiresTemporalDeploymentWhenVersioningEnabled(t *testing.T) {
	validEnvironment(t)
	t.Setenv("TEMPORAL_WORKER_DEPLOYMENT_NAME", "   ")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TEMPORAL_WORKER_DEPLOYMENT_NAME") {
		t.Fatalf("Load() error = %v, want missing deployment name", err)
	}
}

func TestLoadRejectsPartialAuditConfiguration(t *testing.T) {
	validEnvironment(t)
	t.Setenv("PLATFORM_AUDIT_CLIENT_ID", "client-id")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "audit configuration") {
		t.Fatalf("Load() error = %v, want partial audit configuration error", err)
	}
}

func TestLoadAllowsRegisteredApplicationWithoutAuditCredentials(t *testing.T) {
	validEnvironment(t)
	t.Setenv("PLATFORM_APPLICATION_CODE", "contract_management")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "dev")

	config, err := Load()

	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.PlatformApplicationCode != "contract_management" {
		t.Fatalf("PlatformApplicationCode = %q", config.PlatformApplicationCode)
	}
	if config.OIDCIDPHint != "basic-platform" {
		t.Fatalf("OIDCIDPHint = %q", config.OIDCIDPHint)
	}
}

func TestLoadRequiresExplicitKeycloakRealmIssuer(t *testing.T) {
	validEnvironment(t)
	t.Setenv("OIDC_ISSUER", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER") {
		t.Fatalf("Load() error = %v, want explicit issuer error", err)
	}
}

func TestLoadRejectsInvalidSessionEncryptionKey(t *testing.T) {
	validEnvironment(t)
	t.Setenv("OIDC_SESSION_ENCRYPTION_KEY_BASE64", base64.StdEncoding.EncodeToString([]byte("too-short")))
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OIDC_SESSION_ENCRYPTION_KEY_BASE64") {
		t.Fatalf("Load() error = %v, want encryption key error", err)
	}
}

func TestLoadRejectsPlatformURLWithPath(t *testing.T) {
	validEnvironment(t)
	t.Setenv("PLATFORM_BASE_URL", "https://platform.example.com/internal")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PLATFORM_BASE_URL") {
		t.Fatalf("Load() error = %v, want invalid platform URL error", err)
	}
}

func TestProjectIntegrationRequiresHTTPOrigin(t *testing.T) {
	validEnvironment(t)
	t.Setenv("PROJECT_INTEGRATION_ENABLED", "true")
	t.Setenv("PROJECT_API_BASE_URL", "http://project-api:8082/internal")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PROJECT_API_BASE_URL") {
		t.Fatalf("URL error = %v", err)
	}
}

func TestLoadRejectsDashboardMachineWithoutBearer(t *testing.T) {
	validEnvironment(t)
	t.Setenv("DASHBOARD_MACHINE_ENABLED", "true")
	t.Setenv("DASHBOARD_MACHINE_REQUIRE_BEARER", "false")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DASHBOARD_MACHINE_REQUIRE_BEARER") {
		t.Fatalf("Load() error = %v, want dashboard bearer enforcement error", err)
	}
}

func TestLoadRejectsDashboardMachineBearerMissingCredentials(t *testing.T) {
	validEnvironment(t)
	t.Setenv("DASHBOARD_MACHINE_ENABLED", "true")
	t.Setenv("DASHBOARD_MACHINE_REQUIRE_BEARER", "true")
	t.Setenv("DASHBOARD_MACHINE_CLIENT_ID", "")
	t.Setenv("DASHBOARD_MACHINE_AUDIENCE", "")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DASHBOARD_MACHINE_CLIENT_ID") {
		t.Fatalf("Load() error = %v, want missing dashboard client ID error", err)
	}
}

func TestLoadAcceptsDashboardMachineWithBearer(t *testing.T) {
	validEnvironment(t)
	t.Setenv("DASHBOARD_MACHINE_ENABLED", "true")
	t.Setenv("DASHBOARD_MACHINE_REQUIRE_BEARER", "true")
	t.Setenv("DASHBOARD_MACHINE_CLIENT_ID", "data-analysis")
	t.Setenv("DASHBOARD_MACHINE_AUDIENCE", "basic-platform-application")
	t.Setenv("DASHBOARD_MACHINE_ISSUER", "basic-platform")
	t.Setenv("DASHBOARD_MACHINE_PUBLIC_KEY_PATH", "/tmp/application-jwt-public.pem")
	t.Setenv("DASHBOARD_MACHINE_CALLER_APPLICATION_CODE", "data_analysis")
	t.Setenv("DASHBOARD_MACHINE_CALLER_ENVIRONMENT_CODE", "test")
	t.Setenv("DASHBOARD_MACHINE_REQUIRED_SCOPE", "dashboard.contract.read")

	if _, err := Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
}
