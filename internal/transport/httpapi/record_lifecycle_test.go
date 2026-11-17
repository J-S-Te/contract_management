package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestContractRecordManagementRequiresExplicitAdmin 验证记录管理拒绝角色或精确授权不足。
// 参数：t 为测试上下文，各场景分别缺少管理员角色、精确权限或数据范围。
// 返回值：无；接口未按权限拒绝、意外访问空仓储或返回内部错误时失败。
func TestContractRecordManagementRequiresExplicitAdmin(t *testing.T) {
	cases := []struct {
		name, role                   string
		permission, readScope, scope bool
	}{
		{"sales_with_delete_permission", "sales", true, true, true},
		{"audit_admin_with_delete_permission", "audit_admin", true, true, true},
		{"admin_with_only_all", "admin", false, true, true},
		{"admin_without_delete_scope", "admin", true, true, false},
		{"admin_without_read_scope", "admin", true, false, true},
	}
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/contracts/C/deletion-preview"},
		{http.MethodDelete, "/api/v1/contracts/C"},
		{http.MethodPost, "/api/v1/contracts/C/restore"},
	}
	for _, tc := range cases {
		for _, route := range routes {
			t.Run(tc.name+"/"+route.method, func(t *testing.T) {
				actor := application.Principal{
					TenantID: "tenant", UserID: "user", IdentityID: "identity",
					Roles: []string{tc.role},
					Permissions: map[string]bool{
						"contract.read": true, "contract.delete": tc.permission, "all": true,
					},
					PermissionScopes: map[string]contract.ScopeFilter{
						"contract.read":   {AllowAll: tc.readScope},
						"contract.delete": {AllowAll: tc.scope},
					},
				}
				identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
					return actor, nil
				})
				router := NewRouter(&application.Service{}, identity, nil)
				var body *strings.Reader
				body = strings.NewReader(`{"expected_version":1,"reason":"误建合同"}`)
				request := httptest.NewRequest(route.method, route.path, body)
				request.Header.Set("Origin", "http://example.com")
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatalf("status=%d body=%s; want 403", response.Code, response.Body.String())
				}
			})
		}
	}
}
