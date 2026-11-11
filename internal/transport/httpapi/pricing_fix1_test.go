package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

type fix1OrgPriceRepository struct {
	externalContractRepository
	price contract.StandardPrice
}

// GetStandardPrice 提供租户绑定的组织发布价目录替身。
// 参数：ctx为请求，tenant/id为可信查询边界。
// 返回值：隔离组织价；边界不匹配失败关闭。
func (r *fix1OrgPriceRepository) GetStandardPrice(_ context.Context, tenant, id string) (contract.StandardPrice, error) {
	if tenant != r.price.TenantID || id != r.price.ID {
		return contract.StandardPrice{}, application.ErrForbidden
	}
	return r.price, nil
}

// ListStandardPrices 保持测试目录接口完整，不参与本HTTP创建用例。
// 参数：请求、范围、状态和分页。
// 返回值：隔离单价目录。
func (r *fix1OrgPriceRepository) ListStandardPrices(context.Context, contract.ScopeFilter, string, int, int) ([]contract.StandardPrice, error) {
	return []contract.StandardPrice{r.price}, nil
}

// SaveStandardPrice 禁止HTTP创建回归修改价格目录。
// 参数：请求、业务价格、版本、操作者和动作。
// 返回值：固定禁止错误，任何意外写入都会失败。
func (r *fix1OrgPriceRepository) SaveStandardPrice(context.Context, contract.StandardPrice, uint64, string, string) (contract.StandardPrice, error) {
	return contract.StandardPrice{}, application.ErrForbidden
}

type fix1OrgDirectory struct {
	orgs  []string
	calls int
}

// CurrentCatalogOrganizations 记录对原平台本人有效组织目录端口的调用。
// 参数：ctx与actor为可信调用边界。
// 返回值：本场景明确的真实会员组织替身。
func (d *fix1OrgDirectory) CurrentCatalogOrganizations(context.Context, application.Principal) ([]string, error) {
	d.calls++
	return d.orgs, nil
}

// ListCatalogTargets 不授权通过管理目录替代本人组织核验。
// 参数：请求与主体。
// 返回值：禁止错误。
func (*fix1OrgDirectory) ListCatalogTargets(context.Context, application.Principal) ([]contract.LocalCatalogTarget, error) {
	return nil, application.ErrForbidden
}

// TestFix1OrganizationPriceFlowsThroughBothCreateDTOs 以实际Gin、JSON与multipart验证组织价可创建本人合同。
// 参数：t为测试上下文；平台目录与价格仓储为边界替身，不伪造真实SSO。
// 返回值：无；归属须来自权威本人组织，SELF权限不能被扩大。
func TestFix1OrganizationPriceFlowsThroughBothCreateDTOs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, external := range []bool{false, true} {
		for _, member := range []bool{true, false} {
			name := map[bool]string{false: "template", true: "external"}[external] + map[bool]string{true: "-member", false: "-not-member"}[member]
			t.Run(name, func(t *testing.T) {
				r := &fix1OrgPriceRepository{price: contract.StandardPrice{ID: "price", TenantID: "tenant", Name: "组织等保价", CategoryID: "等保测评", ServiceType: "等保测评", OrganizationID: "org-a", Unit: "项", UnitPriceMinor: 10000, Currency: "CNY", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, EffectiveFrom: "2020-01-01", Status: "PUBLISHED", Version: 2}}
				directory := &fix1OrgDirectory{orgs: []string{"org-a"}}
				if !member {
					directory.orgs = []string{"org-b"}
				}
				actor := application.Principal{TenantID: "tenant", UserID: "sales", IdentityID: "sales-subject", Roles: []string{"sales"}, Permissions: map[string]bool{"contract.create": true, "contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.create": {AllowSelf: true}, "contract.read": {AllowSelf: true, OrganizationIDs: []string{"org-a"}}}}
				s := &application.Service{Repo: r, Templates: draftHTTPTemplates{item: contracttemplate.Template{ID: "template", TenantID: "tenant", Content: externalContractDOCX(t)}}, DetectionCategories: externalDetectionCategoryDirectory{}, CRMContractReferences: externalCRMReferenceDirectory{}, ContractSources: &application.ContractSourceOptions{Targets: directory}}
				payload := map[string]any{"title": "组织价格创建", "contract_type": "直签", "amount_minor": 8000, "currency": "CNY", "customer_name": "隔离客户", "service_items": []contract.ServiceItem{{ServiceType: "等保测评", Site: "现场", Batch: "第一批", Category: "等保测评", TestMode: "STANDARD"}}, "pricing": contract.Pricing{BasisDate: "2026-01-01", TaxBasis: "INCLUSIVE", TaxRateBPS: 600, Lines: []contract.PricingLine{{ServiceIndex: 0, PriceID: "price", Quantity: "1", AmountMinor: 8000}}}}
				if external {
					payload["crm_customer_id"] = 42
				} else {
					payload["template_id"] = "template"
					payload["template_values"] = map[string]string{}
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/api/v1/contracts", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Origin", "http://example.com")
				if external {
					request = externalContractUploadRequest(t, string(body), "org-price.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", externalContractDOCX(t))
				}
				router := NewRouter(s, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if !member {
					if response.Code != http.StatusForbidden || r.created.ID != "" {
						t.Fatalf("非本人组织未拒绝：status=%d body=%s", response.Code, response.Body.String())
					}
					return
				}
				if response.Code != http.StatusCreated {
					t.Fatalf("组织价创建status=%d body=%s", response.Code, response.Body.String())
				}
				if r.created.OwnerOrgID != "org-a" || r.created.OwnerIdentityID != "sales-subject" || directory.calls != 1 {
					t.Fatalf("未冻结权威归属：org=%s identity=%s calls=%d", r.created.OwnerOrgID, r.created.OwnerIdentityID, directory.calls)
				}
				if len(actor.PermissionScopes["contract.create"].OrganizationIDs) != 0 || !actor.PermissionScopes["contract.create"].AllowSelf {
					t.Fatal("扩展了SELF权限")
				}
			})
		}
	}
}
