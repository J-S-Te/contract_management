package config

import (
	"strings"
	"testing"
)

// TestImportAutoApproveHistoricalDefaultsToDisabled 验证默认关闭：
// 未配置 CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL 时自动审批保持关闭，
// 历史导入仍走人工会签（不因打开历史/开立开关被顺带打开）。
// 参数：t 为测试上下文。
// 返回值：默认值不是 false 时失败。
func TestImportAutoApproveHistoricalDefaultsToDisabled(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContractImportAutoApproveHistorical {
		t.Fatal("历史导入自动审批默认开启")
	}
}

// TestImportAutoApproveHistoricalFailsClosed 验证依赖与取值口径：
// 历史模式或 CRM 开立通道未开启时不允许开启自动审批（终态归档依赖开立链路），
// 非法布尔值只回报变量名，不泄露配置值。
// 参数：t 为测试上下文。
// 返回值：缺失依赖被接受或错误不含变量名时失败。
func TestImportAutoApproveHistoricalFailsClosed(t *testing.T) {
	t.Run("history-disabled", func(t *testing.T) {
		validEnvironment(t)
		t.Setenv("CONTRACT_IMPORT_HISTORY_ENABLED", "false")
		t.Setenv("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL", "true")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "CONTRACT_IMPORT_HISTORY_ENABLED") {
			t.Fatalf("Load() error = %v, want history dependency error", err)
		}
	})
	t.Run("provision-disabled", func(t *testing.T) {
		validEnvironment(t)
		t.Setenv("CONTRACT_IMPORT_HISTORY_ENABLED", "true")
		t.Setenv("CONTRACT_IMPORT_CONFIRMATION_ROLES", "admin")
		t.Setenv("CONTRACT_IMPORT_CRM_PROVISION_ENABLED", "false")
		t.Setenv("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL", "true")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "CONTRACT_IMPORT_CRM_PROVISION_ENABLED") {
			t.Fatalf("Load() error = %v, want provision dependency error", err)
		}
	})
	t.Run("invalid-boolean", func(t *testing.T) {
		validEnvironment(t)
		t.Setenv("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL", "not-a-bool")
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL") {
			t.Fatalf("Load() error = %v, want boolean error", err)
		}
		// 错误只含键名：不回显配置值。
		if strings.Contains(err.Error(), "not-a-bool") {
			t.Fatalf("错误泄露配置值: %v", err)
		}
	})
}

// TestImportAutoApproveHistoricalAcceptsCompleteConfiguration 验证依赖齐全时开启成功。
// 参数：t 为测试上下文。
// 返回值：合法配置被拒绝或未置位时失败。
func TestImportAutoApproveHistoricalAcceptsCompleteConfiguration(t *testing.T) {
	validEnvironment(t)
	crmProvisionEnvironment(t)
	t.Setenv("CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ContractImportAutoApproveHistorical {
		t.Fatal("历史导入自动审批未按配置开启")
	}
	if !cfg.ContractImportHistoryEnabled || !cfg.ContractImportCRMProvisionEnabled {
		t.Fatalf("依赖开关未开启: history=%t provision=%t",
			cfg.ContractImportHistoryEnabled, cfg.ContractImportCRMProvisionEnabled)
	}
}
