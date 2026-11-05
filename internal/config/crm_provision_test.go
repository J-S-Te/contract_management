package config

import (
	"strings"
	"testing"
)

// crmProvisionEnvironment 打开历史导入 CRM 开立所需的全部开关与两对专用凭据。
// 参数：t 为测试上下文。
// 返回值：无；只写进程环境，不访问任何服务。
func crmProvisionEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "true")
	t.Setenv("CONTRACT_IMPORT_HISTORY_ENABLED", "true")
	t.Setenv("CONTRACT_IMPORT_CONFIRMATION_ROLES", "admin")
	t.Setenv("CRM_PROVISION_BASE_URL", "http://localhost:8090/customer-opportunity")
	t.Setenv("CRM_PROVISION_TOKEN_URL", "http://localhost:8080/oauth2/token")
	t.Setenv("CRM_PROVISION_CLIENT_ID", "contract-import-provision-customer")
	t.Setenv("CRM_PROVISION_CLIENT_SECRET", "test-provision-customer-secret")
	t.Setenv("CRM_PROVISION_SCOPE", "customer.import_provision")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_ID", "contract-import-provision-opportunity")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET", "test-provision-opportunity-secret")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_SCOPE", "opportunity.import_provision")
}

// TestCRMProvisionDefaultsKeepFeatureClosed 验证默认关闭、两链路 scope 各自单值、
// 地址回退 CRM 引用边地址。
// 参数：t 为测试上下文。
// 返回值：无；默认值不符时失败。
func TestCRMProvisionDefaultsKeepFeatureClosed(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "")
	t.Setenv("CRM_PROVISION_BASE_URL", "")
	t.Setenv("CRM_PROVISION_SCOPE", "")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_SCOPE", "")
	t.Setenv("CRM_PROVISION_TOKEN_URL", "")
	t.Setenv("CRM_PROVISION_CLIENT_ID", "")
	t.Setenv("CRM_PROVISION_CLIENT_SECRET", "")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_ID", "")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET", "")
	t.Setenv("CRM_REFERENCE_BASE_URL", "http://crm.local/customer-opportunity")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContractImportCRMProvisionEnabled {
		t.Fatal("CRM provision is enabled by default")
	}
	if cfg.CRMProvisionBaseURL != "http://crm.local/customer-opportunity" {
		t.Fatalf("base url fallback=%q", cfg.CRMProvisionBaseURL)
	}
	if cfg.CRMProvisionScope != "customer.import_provision" {
		t.Fatalf("default customer scope=%q", cfg.CRMProvisionScope)
	}
	if cfg.CRMProvisionOpportunityScope != "opportunity.import_provision" {
		t.Fatalf("default opportunity scope=%q", cfg.CRMProvisionOpportunityScope)
	}
}

// TestCRMProvisionAcceptsCompleteConfiguration 验证开启后两对凭据齐全时通过加载。
// 参数：t 为测试上下文。
// 返回值：无；合法配置被拒绝时失败。
func TestCRMProvisionAcceptsCompleteConfiguration(t *testing.T) {
	validEnvironment(t)
	crmProvisionEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ContractImportCRMProvisionEnabled ||
		cfg.CRMProvisionClientID != "contract-import-provision-customer" ||
		cfg.CRMProvisionClientSecret != "test-provision-customer-secret" ||
		cfg.CRMProvisionOpportunityClientID != "contract-import-provision-opportunity" ||
		cfg.CRMProvisionOpportunityClientSecret != "test-provision-opportunity-secret" ||
		cfg.CRMProvisionTokenURL != "http://localhost:8080/oauth2/token" ||
		cfg.CRMProvisionScope != "customer.import_provision" ||
		cfg.CRMProvisionOpportunityScope != "opportunity.import_provision" {
		t.Fatalf("loaded provision config=%+v", cfg)
	}
}

// TestCRMProvisionFailsClosed 验证开启后的失败关闭口径：历史模式、地址、令牌端点、
// 两对机器凭据（缺失/占位）、两个单值 scope 与布尔解析任一不满足即启动失败。
// 参数：t 为测试上下文。
// 返回值：无；缺失配置被接受时失败。
func TestCRMProvisionFailsClosed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*testing.T)
		want   string
	}{
		{name: "history-disabled", want: "CONTRACT_IMPORT_HISTORY_ENABLED", mutate: func(t *testing.T) {
			t.Setenv("CONTRACT_IMPORT_HISTORY_ENABLED", "false")
			t.Setenv("CONTRACT_IMPORT_CONFIRMATION_ROLES", "")
		}},
		{name: "missing-token-url", want: "CRM_PROVISION_TOKEN_URL", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_TOKEN_URL", "")
		}},
		{name: "missing-customer-client-id", want: "CRM_PROVISION_CLIENT_ID", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_CLIENT_ID", "")
		}},
		{name: "placeholder-customer-secret", want: "CRM_PROVISION_CLIENT_SECRET", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_CLIENT_SECRET", "CHANGEME")
		}},
		{name: "missing-opportunity-client-id", want: "CRM_PROVISION_OPPORTUNITY_CLIENT_ID", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_ID", "")
		}},
		{name: "missing-opportunity-secret", want: "CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET", "")
		}},
		{name: "placeholder-opportunity-client-id", want: "CRM_PROVISION_OPPORTUNITY_CLIENT_ID", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_ID", "PENDING-client")
		}},
		{name: "invalid-base-url", want: "CRM_PROVISION_BASE_URL", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_BASE_URL", "http://localhost:8090/customer-opportunity?x=1")
		}},
		{name: "oauth-style-token-url", want: "CRM_PROVISION_TOKEN_URL", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_TOKEN_URL", "http://user:pass@localhost:8080/oauth2/token")
		}},
		{name: "customer-multi-scope", want: "CRM_PROVISION_SCOPE", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_SCOPE", "customer.import_provision opportunity.import_provision")
		}},
		{name: "customer-wrong-scope", want: "CRM_PROVISION_SCOPE", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_SCOPE", "opportunity.import_provision")
		}},
		{name: "opportunity-multi-scope", want: "CRM_PROVISION_OPPORTUNITY_SCOPE", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_OPPORTUNITY_SCOPE", "customer.import_provision opportunity.import_provision")
		}},
		{name: "opportunity-wrong-scope", want: "CRM_PROVISION_OPPORTUNITY_SCOPE", mutate: func(t *testing.T) {
			t.Setenv("CRM_PROVISION_OPPORTUNITY_SCOPE", "opportunity.signed.write")
		}},
		{name: "invalid-flag", want: "CONTRACT_IMPORT_CRM_PROVISION_ENABLED", mutate: func(t *testing.T) {
			t.Setenv("CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "maybe")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			validEnvironment(t)
			crmProvisionEnvironment(t)
			testCase.mutate(t)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Load() error=%v want substring %q", err, testCase.want)
			}
		})
	}
}

// TestCRMProvisionDisabledIgnoresIncompleteCredentials 验证关闭时两对凭据的残留
// 配置与非法 scope 不影响启动。
// 参数：t 为测试上下文。
// 返回值：无；关闭态被残缺凭据阻断时失败。
func TestCRMProvisionDisabledIgnoresIncompleteCredentials(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "false")
	t.Setenv("CRM_PROVISION_BASE_URL", "not-a-url")
	t.Setenv("CRM_PROVISION_TOKEN_URL", "not-a-url")
	t.Setenv("CRM_PROVISION_CLIENT_ID", "CHANGEME")
	t.Setenv("CRM_PROVISION_CLIENT_SECRET", "")
	t.Setenv("CRM_PROVISION_SCOPE", "customer.import_provision opportunity.import_provision")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_ID", "PENDING")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_CLIENT_SECRET", "")
	t.Setenv("CRM_PROVISION_OPPORTUNITY_SCOPE", "opportunity.signed.write")
	if _, err := Load(); err != nil {
		t.Fatalf("disabled provision must not validate credentials: %v", err)
	}
}
