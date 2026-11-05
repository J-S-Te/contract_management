package config

import (
	"reflect"
	"testing"
)

// TestLocalSourceConfigCompatibility 验证默认CRM/项目兼容与本地联动冲突失败关闭。
// 参数：t为测试上下文，不连接任何配置中的数据库地址。
// 返回值：无。
func TestLocalSourceConfigCompatibility(t *testing.T) {
	validEnvironment(t)
	t.Setenv("CONTRACT_COUNTERPARTY_SOURCE", "")
	t.Setenv("CONTRACT_CATEGORY_SOURCE", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	field := reflect.ValueOf(cfg).FieldByName("CounterpartySource")
	if !field.IsValid() || field.String() != "crm" {
		t.Fatal("default CRM source missing")
	}
	t.Setenv("CONTRACT_COUNTERPARTY_SOURCE", "local")
	t.Setenv("CONTRACT_CATEGORY_SOURCE", "local")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	field = reflect.ValueOf(cfg).FieldByName("CategorySource")
	if !field.IsValid() || field.String() != "local" {
		t.Fatal("local category source missing")
	}
	t.Setenv("CONTRACT_COUNTERPARTY_SOURCE", "typo")
	if _, err := Load(); err == nil {
		t.Fatal("invalid source accepted")
	}
}
