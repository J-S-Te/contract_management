package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

// 模板合同只要带非空 CRM 引用，就必须经过权威 CRM 目录；付款计划缺失不能成为跳过校验的理由。
func TestTemplateContractCRMReferenceIsValidatedWithoutPaymentTerms(t *testing.T) {
	templates := &memoryTemplateRepository{items: map[string]contracttemplate.Template{}}
	template := contracttemplate.Template{
		ID: "template-1", TenantID: "tenant-1", Name: "标准模板",
		Content: applicationTestDOCX(t, "合同正文"),
	}
	templates.items[template.ID] = template
	service := &Service{
		Templates:             templates,
		CRMContractReferences: crmReferenceDirectoryStub{err: ErrCRMReferenceInvalid},
	}
	actor := Principal{
		TenantID: "tenant-1", UserID: "user-1", IdentityID: "user-1",
		Permissions:      map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	input := contract.Contract{
		Title: "模板合同", Type: "直签", TemplateID: template.ID,
		CRMCustomerID: 7, OpportunityID: "123",
		AmountMinor: 100000, Currency: "CNY",
		ServiceItems: []contract.ServiceItem{{
			ServiceType: "等保测评", Systems: []contract.SystemInfo{{Name: "核心系统", Level: "一级"}},
		}},
	}

	_, err := service.prepareContract(context.Background(), actor, input, false, "contract.create")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("prepareContract() error = %v, want ErrValidation from authoritative CRM check", err)
	}
}
