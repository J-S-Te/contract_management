package config

import (
	"strings"
	"testing"
)

// settlementMachineEnvironment 按部署口径写入结算读取机器身份的完整绑定。
// scope 值只是配置校验用例的占位串：结算读取接口的权威 scope 名尚待平台/结算
// 联调下发，测试不据此推断线上名称。
func settlementMachineEnvironment(t *testing.T, scope string) {
	t.Helper()
	t.Setenv("SETTLEMENT_MACHINE_ENABLED", "true")
	t.Setenv("SETTLEMENT_MACHINE_REQUIRE_BEARER", "true")
	t.Setenv("SETTLEMENT_MACHINE_CLIENT_ID", "settlement-contract-reader")
	t.Setenv("SETTLEMENT_MACHINE_AUDIENCE", "settlement-audience")
	t.Setenv("SETTLEMENT_MACHINE_REQUIRED_SCOPE", scope)
}

// 结算读取端点默认关闭：没有显式开关时不能注册跨系统机器接口。
func TestLoadDisablesSettlementMachineByDefault(t *testing.T) {
	validEnvironment(t)
	t.Setenv("SETTLEMENT_MACHINE_ENABLED", "false")
	t.Setenv("SETTLEMENT_MACHINE_REQUIRE_BEARER", "false")
	t.Setenv("SETTLEMENT_MACHINE_REQUIRED_SCOPE", "")
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.SettlementMachineEnabled || config.SettlementMachineRequireBearer {
		t.Fatalf("settlement machine config = %+v, want disabled by default", config)
	}
	if config.SettlementMachineRequiredScope != "" {
		t.Fatalf("settlement machine scope = %q, want empty when disabled", config.SettlementMachineRequiredScope)
	}
}

// 唯一 scope 必须进入配置，供 Keycloak 校验器做“恰好一个范围”的断言。
func TestLoadAcceptsSettlementMachineScopeBinding(t *testing.T) {
	validEnvironment(t)
	settlementMachineEnvironment(t, "settlement.contract.completed.read")
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !config.SettlementMachineEnabled || !config.SettlementMachineRequireBearer {
		t.Fatalf("settlement machine enabled = %+v", config)
	}
	if config.SettlementMachineClientID != "settlement-contract-reader" ||
		config.SettlementMachineAudience != "settlement-audience" ||
		config.SettlementMachineRequiredScope != "settlement.contract.completed.read" {
		t.Fatalf("settlement machine binding = %+v", config)
	}
}

// 开启 Bearer 校验却不绑定范围会让端点退回只校验 client_id/audience 的弱断言，
// 必须在启动期失败关闭。
func TestLoadRejectsSettlementMachineWithoutRequiredScope(t *testing.T) {
	validEnvironment(t)
	settlementMachineEnvironment(t, "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SETTLEMENT_MACHINE_REQUIRED_SCOPE") {
		t.Fatalf("Load() error = %v, want settlement required scope error", err)
	}
}

// 占位值（PENDING/CHANGEME/EXAMPLE.COM）不能被当成有效 scope 放行。
func TestLoadRejectsSettlementMachinePlaceholderScope(t *testing.T) {
	validEnvironment(t)
	settlementMachineEnvironment(t, "PENDING_SCOPE")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "SETTLEMENT_MACHINE_REQUIRED_SCOPE") {
		t.Fatalf("Load() error = %v, want settlement required scope error", err)
	}
}
