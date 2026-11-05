package config

import (
	"reflect"
	"testing"
)

// TestCRMLinkFlagKeepsLegacyAndExplicitDisable 验证缺省按URL兼容、显式关闭停该边及启用失败关闭。
// 参数：t为测试上下文，不访问所配置服务。
// 返回值：无。
func TestCRMLinkFlagKeepsLegacyAndExplicitDisable(t *testing.T) {
	validEnvironment(t)
	crmLinkEnvironment(t)
	t.Setenv("CRM_LINK_ENABLED", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	field := reflect.ValueOf(cfg).FieldByName("CRMLinkEnabled")
	if !field.IsValid() || !field.Bool() {
		t.Fatal("legacy URL no longer enables callback")
	}
	t.Setenv("CRM_LINK_ENABLED", "false")
	t.Setenv("CONTRACT_COUNTERPARTY_SOURCE", "local")
	t.Setenv("CONTRACT_CATEGORY_SOURCE", "local")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("explicit disabled URL conflicts with LOCAL: %v", err)
	}
	if reflect.ValueOf(cfg).FieldByName("CRMLinkEnabled").Bool() {
		t.Fatal("disabled flag ignored")
	}
	t.Setenv("CRM_LINK_ENABLED", "true")
	t.Setenv("CONTRACT_COUNTERPARTY_SOURCE", "crm")
	t.Setenv("CONTRACT_CATEGORY_SOURCE", "project")
	t.Setenv("CRM_LINK_BASE_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("enabled callback accepted missing URL")
	}
	t.Setenv("CRM_LINK_ENABLED", "typo")
	if _, err := Load(); err == nil {
		t.Fatal("invalid link flag accepted")
	}
}
