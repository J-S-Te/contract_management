package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type pricingMemoryRepository struct {
	recordingRepository
	price contract.StandardPrice
	saves int
}

// ListStandardPrices 提供测试目录事实。
// 参数：请求边界和分页。
// 返回值：单条真实测试目录。
func (r *pricingMemoryRepository) ListStandardPrices(context.Context, contract.ScopeFilter, string, int, int) ([]contract.StandardPrice, error) {
	return []contract.StandardPrice{r.price}, nil
}

// GetStandardPrice 返回测试租户的目录版本。
// 参数：请求上下文、租户和价格身份。
// 返回值：冻结测试目录；跨租户或身份不符返回禁止。
func (r *pricingMemoryRepository) GetStandardPrice(_ context.Context, tenant, id string) (contract.StandardPrice, error) {
	if tenant != r.price.TenantID || id != r.price.ID {
		return contract.StandardPrice{}, ErrForbidden
	}
	return r.price, nil
}

// SaveStandardPrice 记录应用服务实际保存行为。
// 参数：上下文、价格、版本、操作者与命令。
// 返回值：输入价格。
func (r *pricingMemoryRepository) SaveStandardPrice(_ context.Context, p contract.StandardPrice, _ uint64, _, _ string) (contract.StandardPrice, error) {
	r.saves++
	return p, nil
}

// pricingFixture 构建已发布价格和正常双行合同。
// 参数：无。
// 返回值：服务、目录仓储、主体与待定价合同。
func pricingFixture() (*Service, *pricingMemoryRepository, Principal, contract.Contract) {
	p := contract.StandardPrice{ID: "price", TenantID: "tenant", Name: "标准软件测试", CategoryID: "category", ServiceType: "软件测试", Unit: "项", UnitPriceMinor: 10000, Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, EffectiveFrom: "2020-01-01", Status: "PUBLISHED", Version: 2, CreatedBy: "author", UpdatedBy: "author"}
	r := &pricingMemoryRepository{price: p}
	s := &Service{Repo: r}
	a := Principal{TenantID: "tenant", UserID: "sales", IdentityID: "sales"}
	c := contract.Contract{Currency: "CNY", AmountMinor: 16000, ServiceItems: []contract.ServiceItem{{SourceID: "a", CategoryID: "category", ServiceType: "软件测试"}, {SourceID: "b", CategoryID: "category", ServiceType: "软件测试"}}, Pricing: &contract.Pricing{BasisDate: "2026-01-01", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, Lines: []contract.PricingLine{{ServiceIndex: 0, PriceID: "price", Quantity: "1", AmountMinor: 8000}, {ServiceIndex: 1, PriceID: "price", Quantity: "1", AmountMinor: 8000}}}}
	return s, r, a, c
}

// TestPricingFreezePreservesReorderedLines 验证重排请求行不改变稳定服务身份。
// 参数：t为测试上下文。
// 返回值：无；目录停用后沿旧冻结价仍须成功。
func TestPricingFreezePreservesReorderedLines(t *testing.T) {
	s, r, a, c := pricingFixture()
	frozen, err := s.preparePricing(context.Background(), a, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.price.Status = "DISABLED"
	r.price.UnitPriceMinor = 90000
	old := frozen.Pricing
	next := preservePricingAmounts(old, 18000, 16000)
	next.Lines[0], next.Lines[1] = next.Lines[1], next.Lines[0]
	frozen.Pricing = next
	frozen.AmountMinor = 18000
	got, err := s.preparePricing(context.Background(), a, frozen, old)
	if err != nil {
		t.Fatalf("目录停用或行重排不应改变冻结基准：%v", err)
	}
	total, err := contract.PricingTotal(got)
	if err != nil || total != 20000 || got.Pricing.Lines[0].ServiceID != "b" {
		t.Fatalf("冻结结果不符：%+v %v", got.Pricing, err)
	}
}

// TestPricingRejectsInvalidCoverage 验证金额守恒和币税、精度及发布生效条件。
// 参数：t为测试上下文。
// 返回值：无；任何非法输入被实际服务拒绝。
func TestPricingRejectsInvalidCoverage(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*pricingMemoryRepository, *contract.Contract)
	}{
		{"draft", func(r *pricingMemoryRepository, c *contract.Contract) { r.price.Status = "DRAFT" }},
		{"currency", func(r *pricingMemoryRepository, c *contract.Contract) { r.price.Currency = "USD" }},
		{"tax", func(r *pricingMemoryRepository, c *contract.Contract) { r.price.TaxBasis = "EXCLUSIVE" }},
		{"expired", func(r *pricingMemoryRepository, c *contract.Contract) { r.price.EffectiveTo = "2025-01-01" }},
		{"precision", func(r *pricingMemoryRepository, c *contract.Contract) { c.Pricing.Lines[0].Quantity = "1.00001" }},
		{"uncovered", func(r *pricingMemoryRepository, c *contract.Contract) { c.Pricing.Lines = c.Pricing.Lines[:1] }},
		{"sum", func(r *pricingMemoryRepository, c *contract.Contract) { c.Pricing.Lines[0].AmountMinor++ }},
		{"duplicate", func(r *pricingMemoryRepository, c *contract.Contract) { c.Pricing.Lines[1].ServiceIndex = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, r, a, c := pricingFixture()
			tc.mutate(r, &c)
			if _, err := s.preparePricing(context.Background(), a, c, nil); err == nil {
				t.Fatal("非法输入被接受")
			}
		})
	}
}

// TestPricingPublisherRequiresIndependentExplicitDuty 验证显式发布权限、角色和非作者。
// 参数：t为测试上下文。
// 返回值：无；越权或自审不会触发保存。
func TestPricingPublisherRequiresIndependentExplicitDuty(t *testing.T) {
	for _, tc := range []struct {
		name, role, user string
		grant            bool
		want             bool
	}{{"admin", "admin", "other", true, false}, {"self", "sales_director", "author", true, false}, {"no permission", "finance_director", "other", false, false}, {"independent", "sales_director", "other", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, r, a, _ := pricingFixture()
			a.Roles = []string{tc.role}
			a.UserID = tc.user
			a.Permissions = map[string]bool{"contract.pricing.publish": tc.grant}
			a.PermissionScopes = allowAllScope("contract.pricing.publish")
			_, err := s.SaveStandardPrice(context.Background(), a, r.price, 2, "PUBLISH")
			if (err == nil) != tc.want || !tc.want && r.saves != 0 {
				t.Fatalf("授权结果err=%v saves=%d", err, r.saves)
			}
		})
	}
}

// TestDiscountUnknownAndFrozenMetrics 验证历史未知不套价，冻结服务端额不接受客户端标准额。
// 参数：t为测试上下文。
// 返回值：无；精确金额和unknown原因不符即失败。
func TestDiscountUnknownAndFrozenMetrics(t *testing.T) {
	s, _, a, c := pricingFixture()
	c.ID = "contract"
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.StartDate = &day
	c.Pricing.Lines[0].StandardMinor = 1
	c.Pricing.Lines[0].UnitPriceMinor = 1
	frozen, err := s.preparePricing(context.Background(), a, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	row := DiscountRow(frozen)
	if row.Status != "KNOWN" || row.StandardMinor != 20000 || row.Bucket != "FROM_80_TO_90" {
		t.Fatalf("不符：%+v", row)
	}
	c.Pricing = nil
	unknown := DiscountRow(c)
	if unknown.Status != "UNKNOWN" || unknown.UnknownReason != "MISSING_PRICING" || unknown.Bucket != "" {
		t.Fatalf("历史被猜填：%+v", unknown)
	}
	_, err = s.preparePricing(context.Background(), a, contract.Contract{Pricing: &contract.Pricing{BasisDate: "2099-01-01"}}, nil)
	if !errors.Is(err, ErrValidation) {
		t.Fatal(err)
	}
}

// TestFinancialChangePreservesFrozenBaselineWithoutCatalog 验证默认改额不依赖当前价格目录。
// 参数：t为测试上下文。
// 返回值：无；更新仍须维持冻结标准额和成交行守恒。
func TestFinancialChangePreservesFrozenBaselineWithoutCatalog(t *testing.T) {
	pricingService, _, pricingActor, input := pricingFixture()
	frozen, err := pricingService.preparePricing(context.Background(), pricingActor, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, repo, actor := financialChangeFixture()
	repo.contract.Pricing = frozen.Pricing
	repo.contract.ServiceItems = frozen.ServiceItems
	repo.contract.AmountMinor = 16000
	change := validFinancialChange()
	_, err = service.ChangeContractFinancialTerms(context.Background(), actor, "C1", change)
	if err != nil {
		t.Fatalf("沿旧冻结基准不应读取新目录：%v", err)
	}
	if len(repo.applied) != 1 {
		t.Fatal("未执行财务变更")
	}
	got := repo.applied[0].Pricing
	var actual, standard int64
	for _, line := range got.Lines {
		actual += line.AmountMinor
		standard += line.StandardMinor
	}
	if actual != change.AmountMinor || standard != 20000 || got.BasisDate != "2026-01-01" {
		t.Fatalf("金额或基准改变：%+v", got)
	}
}
