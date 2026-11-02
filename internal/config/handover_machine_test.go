package config

import (
	"strings"
	"testing"
)

// handoverMachineEnvironment 按平台清单写入交接机器的完整信任域：
// 平台自身 ApplicationJWTManager 签发（caller=platform/prod、唯一 scope=handover.machine），
// audience 为平台 application JWT 的 basic-platform-application。
func handoverMachineEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("HANDOVER_MACHINE_ENABLED", "true")
	t.Setenv("HANDOVER_MACHINE_REQUIRE_BEARER", "true")
	t.Setenv("HANDOVER_MACHINE_CLIENT_ID", "platform-handover")
	t.Setenv("HANDOVER_MACHINE_AUDIENCE", "basic-platform-application")
	t.Setenv("HANDOVER_MACHINE_ISSUER", "basic-platform")
	t.Setenv("HANDOVER_MACHINE_PUBLIC_KEY_PATH", "/app/data/keys/jwt-ed25519-public.pem")
	t.Setenv("HANDOVER_MACHINE_CALLER_APPLICATION_CODE", "platform")
	t.Setenv("HANDOVER_MACHINE_CALLER_ENVIRONMENT_CODE", "prod")
	t.Setenv("HANDOVER_MACHINE_REQUIRED_SCOPE", "handover.machine")
}

// 交接协议默认关闭：没有平台清单时不能注册租户级机器端点。
func TestLoadDisablesHandoverMachineByDefault(t *testing.T) {
	validEnvironment(t)
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if config.HandoverMachineEnabled || config.HandoverMachineRequireBearer {
		t.Fatalf("handover machine config = %+v, want disabled by default", config)
	}
}

// 平台签发的 application JWT 信任域必须完整进入配置，供 Ed25519 验签器绑定。
func TestLoadAcceptsHandoverMachineConfiguration(t *testing.T) {
	validEnvironment(t)
	handoverMachineEnvironment(t)
	config, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !config.HandoverMachineEnabled || !config.HandoverMachineRequireBearer {
		t.Fatalf("handover machine enabled = %+v", config)
	}
	if config.HandoverMachineClientID != "platform-handover" ||
		config.HandoverMachineAudience != "basic-platform-application" ||
		config.HandoverMachineIssuer != "basic-platform" ||
		config.HandoverMachinePublicKeyPath != "/app/data/keys/jwt-ed25519-public.pem" ||
		config.HandoverMachineCallerApp != "platform" ||
		config.HandoverMachineCallerEnv != "prod" ||
		config.HandoverMachineScope != "handover.machine" {
		t.Fatalf("handover machine binding = %+v", config)
	}
}

// 启用交接端点却不校验来源令牌会让端点退化为 503 或“无 Bearer + 请求头租户”，
// 必须在启动期失败关闭。
func TestLoadRejectsHandoverMachineWithoutBearer(t *testing.T) {
	validEnvironment(t)
	handoverMachineEnvironment(t)
	t.Setenv("HANDOVER_MACHINE_REQUIRE_BEARER", "false")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "HANDOVER_MACHINE_REQUIRE_BEARER") {
		t.Fatalf("Load() error = %v, want handover bearer enforcement error", err)
	}
}

// 平台验签器要求 issuer/audience/public key/client/caller/scope 齐全；
// 配置层必须先给出稳定的缺项诊断，而不是等到验签器构造时才失败。
func TestLoadRejectsIncompleteHandoverMachineBinding(t *testing.T) {
	for _, missing := range []string{
		"HANDOVER_MACHINE_CLIENT_ID",
		"HANDOVER_MACHINE_AUDIENCE",
		"HANDOVER_MACHINE_ISSUER",
		"HANDOVER_MACHINE_PUBLIC_KEY_PATH",
		"HANDOVER_MACHINE_CALLER_APPLICATION_CODE",
		"HANDOVER_MACHINE_CALLER_ENVIRONMENT_CODE",
		"HANDOVER_MACHINE_REQUIRED_SCOPE",
	} {
		t.Run(missing, func(t *testing.T) {
			validEnvironment(t)
			handoverMachineEnvironment(t)
			t.Setenv(missing, "")

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Load() error = %v, want missing %s error", err, missing)
			}
		})
	}
}
