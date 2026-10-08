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
