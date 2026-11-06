package application

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

func TestSystemContractNumberCannotBeSupplied(t *testing.T) {
	for _, name := range []string{"合同编号", "系统合同编号"} {
		for _, admin := range []bool{false, true} {
			values, err := normalizeTemplateValues([]contracttemplate.Field{{Name: name, Label: name, Default: "FAKE", Locked: true}}, map[string]string{name: "ATTACKER-NUMBER"}, admin)
			if err != nil || values[name] != "【系统合同编号：审批通过后生成】" {
				t.Fatalf("values=%v error=%v", values, err)
			}
		}
	}
}

func TestReservedNumberMarkerCannotBeInjectedThroughOrdinaryField(t *testing.T) {
	for _, locked := range []bool{false, true} {
		_, err := normalizeTemplateValues([]contracttemplate.Field{{Name: "客户名称", Label: "客户名称", Default: "【系统合同编号：审批通过后生成】", Locked: locked}}, nil, false)
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("locked=%v error=%v", locked, err)
		}
	}
}

func TestCreateTemplateContractUsesLedgerAmountAndSystemNumber(t *testing.T) {
	repository := &memoryTemplateRepository{items: map[string]contracttemplate.Template{"template": {ID: "template", TenantID: "tenant", Content: applicationTestDOCX(t, "{{合同金额}} {{系统合同编号}}"), Fields: []contracttemplate.Field{{Name: "合同金额", Label: "合同金额"}, {Name: "系统合同编号", Label: "系统合同编号"}}}}}
	service := &Service{Repo: &recordingRepository{}, Templates: repository}
	actor := Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]bool{"contract.create": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowSelf: true}}}
	created, err := service.CreateContract(context.Background(), actor, contract.Contract{Number: "FAKE", Title: "测试", Type: "直签", AmountMinor: 1000000, TemplateID: "template", TemplateValues: map[string]string{"合同金额": "99", "系统合同编号": "FAKE"}, ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试"}}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Number != "" || created.TemplateValues["合同金额"] != "10000.00" || !strings.Contains(created.Content, "10000.00") || strings.Contains(created.Content, "FAKE") {
		t.Fatalf("contract=%#v", created)
	}
}

func TestReplaceTemplateSourcePreservesSettingsAndRejectsInvalid(t *testing.T) {
	old := applicationTestDOCX(t, "{{客户名称}}")
	repository := &memoryTemplateRepository{items: map[string]contracttemplate.Template{"template": {ID: "template", TenantID: "tenant", Name: "测试模板", OriginalFilename: "old.docx", Content: old, Fields: []contracttemplate.Field{{Name: "客户名称", Label: "客户显示名称", Default: "固定客户", Locked: true}}}}}
	service := &Service{Templates: repository}
	admin := Principal{TenantID: "tenant", Roles: []string{"admin"}}
	if _, err := service.ReplaceTemplateSource(context.Background(), Principal{TenantID: "tenant"}, "template", "new.docx", old); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := service.ReplaceTemplateSource(context.Background(), admin, "template", "new.docx", []byte("bad")); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid: %v", err)
	}
	updated, err := service.ReplaceTemplateSource(context.Background(), admin, "template", "new.docx", applicationTestDOCX(t, "{{客户名称}} {{新增字段}}"))
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != "template" || updated.OriginalFilename != "new.docx" || len(updated.Fields) != 2 || bytes.Equal(updated.Content, old) {
		t.Fatalf("unexpected replacement: %#v", updated)
	}
	for _, field := range updated.Fields {
		if field.Name == "客户名称" && (field.Label != "客户显示名称" || field.Default != "固定客户" || !field.Locked) {
			t.Fatalf("lost settings: %#v", field)
		}
	}
}

// TestNormalizeNumberFormatSharesContractNumberWhitelist 验证模板 number_format 的固定文本
// 与合同编号规则前缀/后缀共用 [A-Za-z0-9._/-] 白名单：占位符允许，其它字符与超长一律 422。
// 参数：t为测试上下文。
// 返回值：无；非法字符被放行或合法格式被拒时失败。
func TestNormalizeNumberFormatSharesContractNumberWhitelist(t *testing.T) {
	for _, value := range []string{
		contracttemplate.DefaultNumberFormat,
		"HT-{YYYY}-{ID8}",
		"A_b.c/d-{YYYYMMDD}-{ID8}",
		"{ID8}",
	} {
		normalized, err := normalizeNumberFormat(value)
		if err != nil || normalized != value {
			t.Fatalf("合法格式 %q 被拒: %q %v", value, normalized, err)
		}
	}
	for _, value := range []string{
		"HT {ID8}",       // 空格不在白名单
		"合同-{ID8}",       // 中文不在白名单
		"HT#{ID8}",       // # 不在白名单
		"HT（{ID8}）",      // 全角括号不在白名单
		"HT-{ID8}-{BAD}", // 不支持的占位符
		"HT-{YYYY}",
		"",
		strings.Repeat("A", 161) + "{ID8}",
	} {
		if _, err := normalizeNumberFormat(value); !errors.Is(err, ErrValidation) {
			t.Fatalf("非法格式 %q error = %v, want ErrValidation", value, err)
		}
	}

	service := &Service{Templates: &memoryTemplateRepository{items: map[string]contracttemplate.Template{
		"template-1": {ID: "template-1", TenantID: "tenant-1", Name: "旧名称"},
	}}}
	if _, err := service.UpdateTemplate(context.Background(), Principal{TenantID: "tenant-1", Roles: []string{"admin"}},
		"template-1", "标准合同", "合同-{ID8}", nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("UpdateTemplate() error = %v, want ErrValidation", err)
	}
}
