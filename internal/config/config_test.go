package config

import (
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
	t.Setenv("OIDC_TENANT_ID", "01J00000000000000000000000")
	t.Setenv("OIDC_SESSION_COOKIE_NAME", "contract_management_session")
	t.Setenv("OIDC_SESSION_COOKIE_SECURE", "false")
	t.Setenv("OIDC_SESSION_TTL", "8h")
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	t.Setenv("APP_PATH_PREFIX", "/contract_management")
	t.Setenv("TEMPORAL_ADDRESS", "localhost:7233")
	t.Setenv("TEMPORAL_NAMESPACE", "default")
	t.Setenv("TEMPORAL_TASK_QUEUE", "contract-management")
	t.Setenv("TEMPORAL_TLS", "false")
	t.Setenv("APPROVAL_NODE_TIMEOUT", "72h")
	t.Setenv("APPROVAL_REMINDER_INTERVAL", "24h")
	t.Setenv("PLATFORM_AUDIT_CLIENT_ID", "")
	t.Setenv("PLATFORM_AUDIT_CLIENT_SECRET", "")
	t.Setenv("PLATFORM_APPLICATION_CODE", "")
	t.Setenv("PLATFORM_ENVIRONMENT_CODE", "")
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
}

func TestLoadRejectsPlatformURLWithPath(t *testing.T) {
	validEnvironment(t)
	t.Setenv("PLATFORM_BASE_URL", "https://platform.example.com/internal")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PLATFORM_BASE_URL") {
		t.Fatalf("Load() error = %v, want invalid platform URL error", err)
	}
}
