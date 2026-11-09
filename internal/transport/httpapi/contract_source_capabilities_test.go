package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestContractSourceCapabilitiesLocalCatalogOnlyWhenLocalSource 验证本地维护和历史负责人目录能力各自匹配来源及权限。
// 参数：t为测试上下文，覆盖本地、混合、外部来源及无管理权限的组合。
// 返回值：无；能力与当前来源或负责人目录权限不符时测试失败。
func TestContractSourceCapabilitiesLocalCatalogOnlyWhenLocalSource(t *testing.T) {
	cases := []struct {
		name         string
		counterparty string
		category     string
		canManage    bool
		wantManage   bool
		wantHistory  bool
	}{
		{name: "local-both", counterparty: "local", category: "local", canManage: true, wantManage: true, wantHistory: true},
		{name: "local-counterparty", counterparty: "local", category: "project", canManage: true, wantManage: true, wantHistory: true},
		{name: "local-category", counterparty: "crm", category: "local", canManage: true, wantManage: true, wantHistory: true},
		{name: "external-both", counterparty: "crm", category: "project", canManage: true, wantManage: false, wantHistory: true},
		{name: "no-permission", counterparty: "local", category: "local", canManage: false, wantManage: false, wantHistory: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			actor := application.Principal{TenantID: "tenant", UserID: "admin", IdentityID: "admin",
				Permissions: map[string]bool{"contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}}}
			if testCase.canManage {
				actor.Permissions["contract.catalog.manage"] = true
				actor.PermissionScopes["contract.catalog.manage"] = contract.ScopeFilter{AllowAll: true}
			}
			service := &application.Service{ContractSources: &application.ContractSourceOptions{
				CounterpartySource: testCase.counterparty, CategorySource: testCase.category}}
			router := NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
				return actor, nil
			}), nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/contract-source-capabilities", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var envelope struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			got, ok := envelope.Data["local_catalog_manage"].(bool)
			if !ok || got != testCase.wantManage {
				t.Fatalf("local_catalog_manage=%v want %v data=%v", envelope.Data["local_catalog_manage"], testCase.wantManage, envelope.Data)
			}
			history, ok := envelope.Data["history_owner_directory"].(bool)
			if !ok || history != testCase.wantHistory {
				t.Fatalf("history_owner_directory=%v want %v", envelope.Data["history_owner_directory"], testCase.wantHistory)
			}
		})
	}
}
