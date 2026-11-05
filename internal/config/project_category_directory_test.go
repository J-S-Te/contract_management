package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestProjectCategoryDirectoryReadOnlyConfig 验证只读类别可独立于出站并严格校验凭据。
// 参数：t为测试上下文，不连接任何运行数据库。
// 返回值：无；合法混合来源不能启动或缺材料被放行时失败。
func TestProjectCategoryDirectoryReadOnlyConfig(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CONTRACT_COUNTERPARTY_SOURCE", "local")
	t.Setenv("CONTRACT_CATEGORY_SOURCE", "project")
	t.Setenv("CRM_LINK_ENABLED", "false")
	t.Setenv("PROJECT_INTEGRATION_ENABLED", "false")
	t.Setenv("PROJECT_CATEGORY_DIRECTORY_ENABLED", "true")
	t.Setenv("PROJECT_INTEGRATION_TOKEN_URL", "https://platform.example/oauth2/token")
	t.Setenv("PROJECT_INTEGRATION_CLIENT_ID", "category-reader")
	t.Setenv("PROJECT_INTEGRATION_CLIENT_SECRET", "test-only-value")
	cfg, err := Load()
	field := reflect.ValueOf(cfg).FieldByName("ProjectCategoryDirectoryEnabled")
	if err != nil || !field.IsValid() || !field.Bool() || cfg.ProjectIntegrationEnabled {
		t.Fatalf("只读类别未独立装配: field=%v err=%v", field, err)
	}
	t.Setenv("PROJECT_INTEGRATION_CLIENT_SECRET", "")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "PROJECT_INTEGRATION_CLIENT_SECRET") {
		t.Fatalf("缺少真实机器材料未拒绝: %v", err)
	}
	t.Setenv("PROJECT_CATEGORY_DIRECTORY_ENABLED", "sometimes")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "PROJECT_CATEGORY_DIRECTORY_ENABLED") {
		t.Fatal(err)
	}
}
