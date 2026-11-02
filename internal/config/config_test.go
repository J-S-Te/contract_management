package config

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
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
	t.Setenv("OIDC_SESSION_IDLE_TTL", "30m")
	t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "8h")
	// 显式清空旧键，避免开发机环境变量污染空闲/绝对期限的兼容性断言。
	t.Setenv("OIDC_SESSION_TTL", "")
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

func TestLoadDisablesTemporalWorkerVersioningByDefault(t *testing.T) {
	validEnvironment(t)
	t.Setenv("TEMPORAL_WORKER_VERSIONING_ENABLED", "")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.TemporalWorkerVersioning {
		t.Fatal("Temporal worker versioning must require an explicit opt-in")
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

func TestContractFinancialChangeSwitchDefaultsToClosed(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CONTRACT_FINANCIAL_CHANGE_ENABLED", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// 受控财务变更入口必须默认关闭：未配置时不得因为零值/缺省而开放。
	if cfg.ContractFinancialChangeEnabled {
		t.Fatal("CONTRACT_FINANCIAL_CHANGE_ENABLED 未配置时必须默认关闭")
	}
	t.Setenv("CONTRACT_FINANCIAL_CHANGE_ENABLED", "true")
	if cfg, err = Load(); err != nil || !cfg.ContractFinancialChangeEnabled {
		t.Fatalf("显式开启未被识别：enabled=%v err=%v", cfg.ContractFinancialChangeEnabled, err)
	}
	t.Setenv("CONTRACT_FINANCIAL_CHANGE_ENABLED", "sometimes")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "CONTRACT_FINANCIAL_CHANGE_ENABLED") {
		t.Fatalf("非法开关值必须报错并定位键名：%v", err)
	}
}

func TestCRMReferenceIntegrationRequiresLeastPrivilegeMachineConfiguration(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CRM_REFERENCE_ENABLED", "true")
	t.Setenv("CRM_REFERENCE_BASE_URL", "http://customer-api:8090")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CRM_REFERENCE_TOKEN_URL") {
		t.Fatalf("missing token configuration error=%v", err)
	}
	t.Setenv("CRM_REFERENCE_TOKEN_URL", "http://platform-api:8080/oauth2/token")
	t.Setenv("CRM_REFERENCE_CLIENT_ID", "contract-crm-reader")
	t.Setenv("CRM_REFERENCE_CLIENT_SECRET", "secret")
	t.Setenv("CRM_REFERENCE_SCOPE", "customer.read")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CRM_REFERENCE_SCOPE") {
		t.Fatalf("overbroad scope error=%v", err)
	}
	t.Setenv("CRM_REFERENCE_SCOPE", "customer.contract_reference.read")
	if _, err := Load(); err != nil {
		t.Fatalf("valid CRM reference configuration error=%v", err)
	}
}

func TestCRMReferenceBaseURLMountedPrefix(t *testing.T) {
	for _, base := range []string{"http://customer-api:8090", "http://customer-api:8090/", "http://customer-api:8090/customer-opportunity", "https://crm.internal/customer-opportunity/"} {
		t.Run(base, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv("CRM_REFERENCE_ENABLED", "true")
			t.Setenv("CRM_REFERENCE_BASE_URL", base)
			t.Setenv("CRM_REFERENCE_TOKEN_URL", "http://platform-api:8080/oauth2/token")
			t.Setenv("CRM_REFERENCE_CLIENT_ID", "contract-crm-reader")
			t.Setenv("CRM_REFERENCE_CLIENT_SECRET", "test-only-secret")
			t.Setenv("CRM_REFERENCE_SCOPE", "customer.contract_reference.read")
			if _, err := Load(); err != nil {
				t.Fatalf("mounted CRM base rejected: %v", err)
			}
		})
	}
	for _, base := range []string{"ftp://crm/customer-opportunity", "http://user:pass@crm/customer-opportunity", "http://crm/customer-opportunity?x=1", "http://crm/customer-opportunity?", "http://crm/customer-opportunity#fragment", "http://crm/arbitrary", "http://crm/customer-opportunity/../internal", "http://crm/customer%2dopportunity"} {
		t.Run(base, func(t *testing.T) {
			if validCRMReferenceBaseURL(base) {
				t.Fatal("unsafe CRM base accepted")
			}
		})
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

// CRM 关联回写凭据（CRM_LINK_*）纳入启动校验。BaseURL 为空表示该边关闭；
// 配置 BaseURL 后必须使用平台 client_credentials，TOKEN_URL/CLIENT_ID/
// CLIENT_SECRET 缺一即失败关闭（不允许静态 Bearer 或无令牌发送）。
func TestLoadTreatsEmptyCRMLinkBaseURLAsDisabled(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CRM_LINK_BASE_URL", "")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.CRMLinkBaseURL != "" || config.CRMLinkTokenURL != "" {
		t.Fatalf("CRMLink = %q/%q, want empty (integration disabled)", config.CRMLinkBaseURL, config.CRMLinkTokenURL)
	}
}

func TestLoadAcceptsValidCRMLinkConfiguration(t *testing.T) {
	validEnvironment(t)
	crmLinkEnvironment(t)

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.CRMLinkBaseURL != "http://customer-api:8090/customer-opportunity" ||
		config.CRMLinkTokenURL != "http://platform-api:8080/oauth2/token" ||
		config.CRMLinkClientID != "contract_management-prod-crm-contract-link" ||
		config.CRMLinkClientSecret != "test-only-crm-link-secret" ||
		config.CRMLinkScope != "opportunity.signed.write" {
		t.Fatalf("CRMLink = %#v, want configured values", config)
	}
}

func TestLoadRejectsNonHTTPCRMLinkBaseURL(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CRM_LINK_BASE_URL", "ftp://customer-api:8090")
	t.Setenv("CRM_LINK_TOKEN_URL", "http://platform-api:8080/oauth2/token")
	t.Setenv("CRM_LINK_CLIENT_ID", "contract_management-prod-crm-contract-link")
	t.Setenv("CRM_LINK_CLIENT_SECRET", "test-only-crm-link-secret")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CRM_LINK_BASE_URL") {
		t.Fatalf("Load() error = %v, want invalid CRM_LINK_BASE_URL origin error", err)
	}
}

func TestLoadRequiresCRMLinkCredentialsWhenBaseURLIsSet(t *testing.T) {
	validEnvironment(t)
	for _, tc := range []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"missing token url", "CRM_LINK_TOKEN_URL", "", "CRM_LINK_TOKEN_URL"},
		{"missing client id", "CRM_LINK_CLIENT_ID", "", "CRM_LINK_CLIENT_ID"},
		{"blank client secret", "CRM_LINK_CLIENT_SECRET", "   ", "CRM_LINK_CLIENT_SECRET"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crmLinkEnvironment(t)
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %v, want missing %s error", err, tc.want)
			}
		})
	}
}

func TestLoadRejectsUnexpectedCRMLinkScope(t *testing.T) {
	validEnvironment(t)
	crmLinkEnvironment(t)
	t.Setenv("CRM_LINK_SCOPE", "opportunity.status.write")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CRM_LINK_SCOPE") {
		t.Fatalf("Load() error = %v, want CRM_LINK_SCOPE error", err)
	}
}

func crmLinkEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("CRM_LINK_BASE_URL", "http://customer-api:8090/customer-opportunity")
	t.Setenv("CRM_LINK_TOKEN_URL", "http://platform-api:8080/oauth2/token")
	t.Setenv("CRM_LINK_CLIENT_ID", "contract_management-prod-crm-contract-link")
	t.Setenv("CRM_LINK_CLIENT_SECRET", "test-only-crm-link-secret")
	t.Setenv("CRM_LINK_SCOPE", "opportunity.signed.write")
}

func contractSummaryEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("CONTRACT_SUMMARY_MACHINE_ENABLED", "true")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER", "true")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_ISSUER", "basic-platform")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_AUDIENCE", "basic-platform-application")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH", "/app/data/keys/jwt-ed25519-public.pem")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_CLIENT_ID", "customer_and_opportunity-dev-contract-summary")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE", "customer_and_opportunity")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_CALLER_ENVIRONMENT_CODE", "dev")
	t.Setenv("CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE", "contract.summary.read")
}

func TestLoadDisablesContractSummaryMachineByDefault(t *testing.T) {
	validEnvironment(t)
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.ContractSummaryMachineEnabled {
		t.Fatal("contract summary machine endpoint must default disabled")
	}
}

func TestLoadAcceptsContractSummaryMachineConfiguration(t *testing.T) {
	validEnvironment(t)
	contractSummaryEnvironment(t)
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !config.ContractSummaryMachineEnabled || config.ContractSummaryMachineScope != "contract.summary.read" ||
		config.ContractSummaryMachineClientID != "customer_and_opportunity-dev-contract-summary" {
		t.Fatalf("contract summary config = %+v", config)
	}
}

func TestLoadRejectsIncompleteContractSummaryMachineConfiguration(t *testing.T) {
	validEnvironment(t)
	contractSummaryEnvironment(t)
	t.Setenv("CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH", "   ")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONTRACT_SUMMARY_MACHINE_PUBLIC_KEY_PATH") {
		t.Fatalf("Load() error = %v, want missing public key path error", err)
	}

	contractSummaryEnvironment(t)
	t.Setenv("CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER", "false")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONTRACT_SUMMARY_MACHINE_REQUIRE_BEARER") {
		t.Fatalf("Load() error = %v, want require-bearer error", err)
	}
}

func TestLoadRejectsContractSummaryScopeOrCallerDrift(t *testing.T) {
	validEnvironment(t)
	contractSummaryEnvironment(t)
	t.Setenv("CONTRACT_SUMMARY_MACHINE_REQUIRED_SCOPE", "contract.read")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "contract.summary.read") {
		t.Fatalf("Load() error = %v, want fixed scope error", err)
	}

	contractSummaryEnvironment(t)
	t.Setenv("CONTRACT_SUMMARY_MACHINE_CALLER_APPLICATION_CODE", "data_analysis")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "customer_and_opportunity") {
		t.Fatalf("Load() error = %v, want fixed caller error", err)
	}
}

// unsetEnvironmentKey 让指定环境变量在用例期间真正不存在，用于验证“未配置取默认值”。
// t.Setenv 只能设置，无法表达“未设置”，因此这里显式 unset 并在清理时还原原值。
func unsetEnvironmentKey(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if !existed {
			_ = os.Unsetenv(key)
			return
		}
		_ = os.Setenv(key, previous)
	})
}

func TestLoadDefaultsNumberAuditCron(t *testing.T) {
	validEnvironment(t)
	unsetEnvironmentKey(t, "NUMBER_AUDIT_CRON_SCHEDULE")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// 默认每天 17:00，比合同归档的 16:00 错开一小时，避免两条 cron 同时抢同一批连接。
	if config.NumberAuditCron != "0 17 * * *" {
		t.Fatalf("NumberAuditCron = %q, want default %q", config.NumberAuditCron, "0 17 * * *")
	}
}

func TestLoadOverridesNumberAuditCron(t *testing.T) {
	validEnvironment(t)
	t.Setenv("NUMBER_AUDIT_CRON_SCHEDULE", "30 3 * * *")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.NumberAuditCron != "30 3 * * *" {
		t.Fatalf("NumberAuditCron = %q, want explicit override", config.NumberAuditCron)
	}
}

// TestLoadDefaultsSessionIdleAndAbsoluteTTL 固定统一会话默认值：空闲 30m、绝对 8h。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadDefaultsSessionIdleAndAbsoluteTTL(t *testing.T) {
	validEnvironment(t)
	unsetEnvironmentKey(t, "OIDC_SESSION_IDLE_TTL")
	unsetEnvironmentKey(t, "OIDC_SESSION_ABSOLUTE_TTL")
	unsetEnvironmentKey(t, "OIDC_SESSION_TTL")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OIDCSessionIdleTTL != 30*time.Minute {
		t.Fatalf("OIDCSessionIdleTTL = %v, want 30m", config.OIDCSessionIdleTTL)
	}
	if config.OIDCSessionAbsoluteTTL != 8*time.Hour {
		t.Fatalf("OIDCSessionAbsoluteTTL = %v, want 8h", config.OIDCSessionAbsoluteTTL)
	}
}

// TestLoadFallsBackToLegacySessionTTLAsAbsolute 验证旧 OIDC_SESSION_TTL 的兼容语义：
// 未设置新绝对期限键时，旧键就是绝对期限，避免旧环境被静默放大到默认 8h。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadFallsBackToLegacySessionTTLAsAbsolute(t *testing.T) {
	validEnvironment(t)
	unsetEnvironmentKey(t, "OIDC_SESSION_ABSOLUTE_TTL")
	t.Setenv("OIDC_SESSION_TTL", "15m")
	t.Setenv("OIDC_SESSION_IDLE_TTL", "5m")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OIDCSessionAbsoluteTTL != 15*time.Minute {
		t.Fatalf("OIDCSessionAbsoluteTTL = %v, want legacy 15m", config.OIDCSessionAbsoluteTTL)
	}
	if config.OIDCSessionIdleTTL != 5*time.Minute {
		t.Fatalf("OIDCSessionIdleTTL = %v, want 5m", config.OIDCSessionIdleTTL)
	}
}

// TestLoadPrefersNewAbsoluteTTLOverLegacy 验证新键与旧键同时存在时以新键为准。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadPrefersNewAbsoluteTTLOverLegacy(t *testing.T) {
	validEnvironment(t)
	t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "8h")
	t.Setenv("OIDC_SESSION_TTL", "15m")

	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.OIDCSessionAbsoluteTTL != 8*time.Hour {
		t.Fatalf("OIDCSessionAbsoluteTTL = %v, want explicit 8h", config.OIDCSessionAbsoluteTTL)
	}
}

// TestLoadRejectsAbsoluteTTLNotLongerThanIdle 验证绝对期限必须严格大于空闲期限。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadRejectsAbsoluteTTLNotLongerThanIdle(t *testing.T) {
	validEnvironment(t)
	t.Setenv("OIDC_SESSION_IDLE_TTL", "8h")
	t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "8h")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OIDC_SESSION_ABSOLUTE_TTL") {
		t.Fatalf("Load() error = %v, want absolute TTL > idle TTL", err)
	}
}

// TestLoadRejectsIdleTTLOutsideAllowedRange 验证空闲期限超出 60s~24h 时启动失败关闭。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadRejectsIdleTTLOutsideAllowedRange(t *testing.T) {
	for _, idle := range []string{"30s", "25h"} {
		validEnvironment(t)
		t.Setenv("OIDC_SESSION_IDLE_TTL", idle)
		t.Setenv("OIDC_SESSION_ABSOLUTE_TTL", "36h")

		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OIDC_SESSION_IDLE_TTL") {
			t.Fatalf("Load() with idle %q error = %v, want idle range error", idle, err)
		}
	}
}

// TestLoadRejectsLegacyShortTTLWithDefaultIdle 验证旧环境只配 15m 绝对期限时不会静默生效：
// 默认空闲 30m 大于绝对期限，配置非法，必须由运维显式改用新键。
// 参数：t 为测试上下文。
// 返回值：无。
func TestLoadRejectsLegacyShortTTLWithDefaultIdle(t *testing.T) {
	validEnvironment(t)
	unsetEnvironmentKey(t, "OIDC_SESSION_ABSOLUTE_TTL")
	unsetEnvironmentKey(t, "OIDC_SESSION_IDLE_TTL")
	t.Setenv("OIDC_SESSION_TTL", "15m")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OIDC_SESSION_ABSOLUTE_TTL") {
		t.Fatalf("Load() error = %v, want fail-closed legacy TTL configuration", err)
	}
}

func TestLoadTreatsEmptyNumberAuditCronAsDisabled(t *testing.T) {
	validEnvironment(t)
	// 显式空串（含纯空白）表示禁用周期巡检；worker 见空值不注册 cron。
	for _, value := range []string{"", "   "} {
		t.Setenv("NUMBER_AUDIT_CRON_SCHEDULE", value)
		config, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if config.NumberAuditCron != "" {
			t.Fatalf("NumberAuditCron = %q for env %q, want empty (disabled)", config.NumberAuditCron, value)
		}
	}
}
