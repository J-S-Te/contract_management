package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// UpdateContractDraft 将真实应用服务准备的草稿保存到测试仓储。
// 参数：ctx为上下文，c为候选草稿，version为原版本，actor为操作者。
// 返回值：旧版本不符返回冲突，成功保存完整候选。
func (r *pricingMemoryRepository) UpdateContractDraft(_ context.Context, c contract.Contract, version uint64, _ string) error {
	if r.contract.Version != version {
		return apperrors.ErrVersionConflict
	}
	c.Version = version + 1
	r.contract = c
	r.saves++
	return nil
}

// pricedDraftFixture 建立经过真实定价冻结的可编辑模板或外部草稿。
// 参数：t为测试上下文，external表示原件模式。
// 返回值：服务、仓储、编辑主体及独立编辑输入。
func pricedDraftFixture(t *testing.T, external bool) (*Service, *pricingMemoryRepository, Principal, contract.Contract) {
	t.Helper()
	s, base, actor, input := draftFixture(t)
	r := &pricingMemoryRepository{recordingRepository: base.recordingRepository, price: contract.StandardPrice{ID: "price", TenantID: "tenant", Name: "软件测试价", CategoryID: "软件测试", ServiceType: "软件测试", Unit: "项", UnitPriceMinor: 10000, Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, EffectiveFrom: "2020-01-01", Status: "PUBLISHED", Version: 2}}
	s.Repo = r
	input.AmountMinor = 8000
	input.Currency = "CNY"
	input.ServiceItems = []contract.ServiceItem{{SourceID: "stable", ServiceType: "软件测试", Category: "软件测试", Site: "现场", Batch: "第一批", TestMode: "STANDARD"}}
	input.Pricing = &contract.Pricing{BasisDate: "2026-01-01", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, Lines: []contract.PricingLine{{ServiceIndex: 0, PriceID: "price", Quantity: "1", AmountMinor: 8000}}}
	frozen, err := s.preparePricing(context.Background(), actor, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.contract.AmountMinor = input.AmountMinor
	r.contract.Currency = input.Currency
	r.contract.ServiceItems = frozen.ServiceItems
	r.contract.Pricing = frozen.Pricing
	if external {
		r.contract.TemplateID = ""
		r.contract.Content = "original"
		r.contract.Document = applicationTestDOCX(t, "original")
		input.TemplateID = ""
		input.Content = "original"
		input.TemplateValues = nil
		input.CRMCustomerID = 9
		input.CustomerName = "client"
		s.CRMContractReferences = validCRMReference(9)
		s.DetectionCategories = enabledDetectionCategories("软件测试")
	}
	data, _ := json.Marshal(frozen.Pricing)
	input.Pricing = new(contract.Pricing)
	_ = json.Unmarshal(data, input.Pricing)
	return s, r, actor, input
}

// TestFix1ExternalDraftSavePreservesFrozenIdentity 覆盖外部草稿真实准备链对普通元数据编辑的身份保留。
// 参数：t为测试上下文；同时覆盖完整pricing和旧客户端省略pricing。
// 返回值：无；普通保存必须保留服务身份、基准日、标准额及冻结时间。
func TestFix1ExternalDraftSavePreservesFrozenIdentity(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "full-pricing", true: "omitted-pricing"}[omit], func(t *testing.T) {
			s, r, a, input := pricedDraftFixture(t, true)
			old := r.contract.Pricing
			if omit {
				input.Pricing = nil
			}
			input.Title = "仅改标题"
			r.price.Status = "DISABLED"
			got, err := s.UpdateContractDraft(context.Background(), a, "C", 3, input)
			if err != nil {
				t.Fatal(err)
			}
			if got.ServiceItems[0].SourceID != "stable" || got.Pricing.Lines[0].LineID != old.Lines[0].LineID || got.Pricing.BasisDate != old.BasisDate || got.Pricing.Lines[0].StandardMinor != 10000 || !got.Pricing.FrozenAt.Equal(old.FrozenAt) {
				t.Fatalf("冻结依据改变：%+v", got.Pricing)
			}
		})
	}
}

// TestFix1DraftCannotRelabelFrozenCurrencyOrService 覆盖模板草稿更换币种或稳定服务业务语义时的拒绝。
// 参数：t为测试上下文；无重选时不得把旧基准改名为新币种或新类别。
// 返回值：无；拒绝前不保存任何新版本。
func TestFix1DraftCannotRelabelFrozenCurrencyOrService(t *testing.T) {
	for _, kind := range []string{"currency", "service-type", "category"} {
		for _, omit := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "-full", true: "-omitted"}[omit], func(t *testing.T) {
				s, r, a, input := pricedDraftFixture(t, false)
				if omit {
					input.Pricing = nil
				}
				switch kind {
				case "currency":
					input.Currency = "USD"
				case "service-type":
					input.ServiceItems[0].ServiceType = "渗透测试"
				case "category":
					input.ServiceItems[0].Category = "另一个合法类别"
				}
				if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, input); !errors.Is(err, ErrValidation) {
					t.Fatalf("改动%s应要求显式重选，得到%v", kind, err)
				}
				if r.saves != 0 {
					t.Fatal("非法变更已写入")
				}
			})
		}
	}
}

// TestFix1DraftCategoryVersionRequiresExplicitReselection 验证权威本地类别升级也不能复用旧价格版本。
// 参数：t为测试上下文，目录端口返回真实选择的新版本边界替身。
// 返回值：无；未显式重选应拒绝且不保存。
func TestFix1DraftCategoryVersionRequiresExplicitReselection(t *testing.T) {
	s, r, a, input := pricedDraftFixture(t, false)
	input.CategorySource = contract.CategoryLocal
	input.ServiceItems[0].CategoryID = "local-category"
	input.ServiceItems[0].CategoryVersion = 3
	r.price.CategoryID = "local-category"
	r.price.CategoryVersion = 3
	initial, err := s.preparePricing(context.Background(), a, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.contract.Pricing = initial.Pricing
	r.contract.CategorySource = contract.CategoryLocal
	r.contract.ServiceItems[0].CategoryID = "local-category"
	r.contract.ServiceItems[0].CategoryVersion = 3
	input.CategorySource = contract.CategoryLocal
	input.ServiceItems[0].CategoryID = "local-category"
	input.ServiceItems[0].CategoryVersion = 4
	s.ContractSources = &ContractSourceOptions{CategorySource: "local", Catalog: &catalogReferenceFixture{category: contract.LocalCategory{ID: "local-category", TenantID: "tenant", Name: "软件测试", Enabled: true, Version: 4}}}
	if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, input); !errors.Is(err, ErrValidation) {
		t.Fatalf("本地类别换版本未要求重选：%v", err)
	}
	if r.saves != 0 {
		t.Fatal("不匹配类别版本被保存")
	}
}
