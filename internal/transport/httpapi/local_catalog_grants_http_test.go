package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type catalogGrantHTTPStore struct {
	application.LocalCatalogRepository
	writes int
}

// ListLocalCatalogGrants 提供当前条目授权历史的受控存储端口。
// 参数：ctx/filter为可信范围，kind/id为条目。
// 返回值：空历史，不模拟真实数据库验收。
func (f *catalogGrantHTTPStore) ListLocalCatalogGrants(ctx context.Context, filter contract.ScopeFilter, kind, id string) ([]contract.LocalCatalogGrant, error) {
	return []contract.LocalCatalogGrant{}, nil
}

// SaveLocalCatalogGrant 捕获HTTP经过真实服务校验后的授权。
// 参数：ctx/filter为可信范围，kind/id为条目，value为已验证目标，expected为版本，actor为操作者。
// 返回值：受控新版本。
func (f *catalogGrantHTTPStore) SaveLocalCatalogGrant(ctx context.Context, filter contract.ScopeFilter, kind, id string, value contract.LocalCatalogGrant, expected uint64, actor string) (contract.LocalCatalogGrant, error) {
	f.writes++
	value.Version = expected + 1
	return value, nil
}

type catalogGrantHTTPDirectory struct{}

// ListCatalogTargets 提供已核验的销售，供真实HTTP服务边界测试。
// 参数：ctx/actor为可信主体。
// 返回值：一个明确有效的测试销售。
func (catalogGrantHTTPDirectory) ListCatalogTargets(ctx context.Context, actor application.Principal) ([]contract.LocalCatalogTarget, error) {
	return []contract.LocalCatalogTarget{{SubjectType: "USER", SubjectID: "sales-valid", Name: "有效销售"}}, nil
}

// CurrentCatalogOrganizations 返回测试主体的当前组织边界。
// 参数：ctx/actor为当前主体。
// 返回值：空会员关系。
func (catalogGrantHTTPDirectory) CurrentCatalogOrganizations(ctx context.Context, actor application.Principal) ([]string, error) {
	return nil, nil
}

// TestCatalogGrantHTTPStrictAndAuthorized 验证真实路由/解码/服务门禁不接纳自由目标或客户端租户。
// 参数：t为测试上下文，认证后的主体由受控中间件提供。
// 返回值：无；权限、白名单、显式启停或权威目标被绕过时失败。
func TestCatalogGrantHTTPStrictAndAuthorized(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		manage     bool
		want       int
	}{
		{"authorized", `{"subject_type":"USER","subject_id":"sales-valid","enabled":true,"expected_version":0}`, true, http.StatusOK},
		{"foreign target", `{"subject_type":"USER","subject_id":"foreign","enabled":true}`, true, http.StatusConflict},
		{"forged tenant", `{"tenant_id":"foreign","subject_type":"USER","subject_id":"sales-valid","enabled":true}`, true, http.StatusUnprocessableEntity},
		{"missing explicit enable", `{"subject_type":"USER","subject_id":"sales-valid"}`, true, http.StatusUnprocessableEntity},
		{"no management", `{"subject_type":"USER","subject_id":"sales-valid","enabled":true}`, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &catalogGrantHTTPStore{}
			h := &Handler{service: &application.Service{ContractSources: &application.ContractSourceOptions{Catalog: store, Targets: catalogGrantHTTPDirectory{}}}}
			router := gin.New()
			group := router.Group("/api/v1", func(c *gin.Context) {
				actor := application.Principal{TenantID: "tenant", UserID: "admin", IdentityID: "admin", Permissions: map[string]bool{}, PermissionScopes: map[string]contract.ScopeFilter{}}
				if tc.manage {
					actor.Permissions["contract.catalog.manage"] = true
					actor.PermissionScopes["contract.catalog.manage"] = contract.ScopeFilter{AllowAll: true}
				}
				c.Set(principalKey, actor)
				c.Next()
			})
			h.mountLocalCatalog(group)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/local-catalog/counterparties/entry/grants", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			expected := 0
			if tc.want == http.StatusOK {
				expected = 1
			}
			if store.writes != expected {
				t.Fatalf("writes=%d", store.writes)
			}
		})
	}
}
