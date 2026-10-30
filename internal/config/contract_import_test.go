package config

import (
	"strings"
	"testing"
	"time"
)

// TestImportConfigurationDefaultsClosed 验证新增入口默认关闭且有界。
// 参数：t 为配置测试上下文。
// 返回值：无；默认开通或限制丢失时测试失败。
func TestImportConfigurationDefaultsClosed(t *testing.T) {
	validEnvironment(t)
	for _, key := range []string{"CONTRACT_EXTERNAL_PDF_ENABLED", "CONTRACT_IMPORT_ENABLED",
		"CONTRACT_IMPORT_HISTORY_ENABLED", "CONTRACT_IMPORT_MAX_ITEMS",
		"CONTRACT_IMPORT_MAX_CONCURRENCY", "CONTRACT_IMPORT_STAGING_TTL"} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExternalPDFEnabled || cfg.ContractImportEnabled || cfg.ContractImportHistoryEnabled ||
		cfg.ContractImportMaxItems != 100 || cfg.ContractImportMaxConcurrency != 1 ||
		cfg.ContractImportStagingTTL != 168*time.Hour {
		t.Fatal("新增入口必须默认关闭且使用有界资源默认值")
	}
}

// TestImportConfigurationRejectsUnsafeLimits 验证非法开关及无界配置不静默降级。
// 参数：t 为配置边界测试上下文。
// 返回值：无；非法值被接受或错误泄露配置值时测试失败。
func TestImportConfigurationRejectsUnsafeLimits(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"CONTRACT_EXTERNAL_PDF_ENABLED", "perhaps"}, {"CONTRACT_IMPORT_ENABLED", "perhaps"},
		{"CONTRACT_IMPORT_HISTORY_ENABLED", "perhaps"}, {"CONTRACT_IMPORT_MAX_ITEMS", "0"},
		{"CONTRACT_IMPORT_MAX_ITEMS", "101"}, {"CONTRACT_IMPORT_MAX_CONCURRENCY", "5"},
		{"CONTRACT_IMPORT_STAGING_TTL", "1h"}, {"CONTRACT_IMPORT_STAGING_TTL", "1000h"},
	} {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			validEnvironment(t)
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tt.key) {
				t.Fatalf("expected bounded %s error, got %v", tt.key, err)
			}
		})
	}
}

// TestHistoryConfigRequiresApprovedRoles 验证历史开关不能隐式创建免审批模板。
// 参数：t为配置测试上下文。
// 返回值：无；缺失或未知角色被接受时失败。
func TestHistoryConfigRequiresApprovedRoles(t *testing.T) {
	for _, roles := range []string{"", "invented_role"} {
		validEnvironment(t)
		t.Setenv("CONTRACT_IMPORT_HISTORY_ENABLED", "true")
		t.Setenv("CONTRACT_IMPORT_CONFIRMATION_ROLES", roles)
		if _, e := Load(); e == nil || !strings.Contains(e.Error(), "CONTRACT_IMPORT_CONFIRMATION_ROLES") {
			t.Fatal("unapproved template accepted", e)
		}
	}
}
