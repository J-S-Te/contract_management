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
)

// TestImportCapabilitiesExposesConfiguredBatchLimit 验证页面读取的上限与服务实际配置一致。
// 参数：t为无需业务库的HTTP测试上下文。
// 返回值：无；三份上限丢失或误报时失败。
func TestImportCapabilitiesExposesConfiguredBatchLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	actor := application.Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]bool{"contract.read": true}}
	router := NewRouter(&application.Service{ContractImportEnabled: true, ContractImportMaxItems: 3},
		identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-source-capabilities", nil))
	var result struct {
		Data struct {
			Max int `json:"max_batch_items"`
		} `json:"data"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Data.Max != 3 {
		t.Fatalf("能力未返回真实批次上限: %d %s", response.Code, response.Body.String())
	}
}

// TestImportBatchLimitResponse 验证超限响应携带实际数量和服务端上限。
// 参数：t为轻量HTTP错误响应测试上下文。
// 返回值：无；仍为通用422或数量错误时失败。
func TestImportBatchLimitResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/contract-import-batches/batch/manifest", nil)
	writeError(c, &application.ImportBatchItemLimitError{Limit: 3, Count: 4})
	var result struct {
		Code    string `json:"code"`
		Details struct {
			Max   int    `json:"max_items"`
			Count int    `json:"actual_items"`
			Field string `json:"field_path"`
		} `json:"details"`
	}
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || response.Code != 422 ||
		result.Code != "IMPORT_BATCH_ITEM_LIMIT" || result.Details.Max != 3 ||
		result.Details.Count != 4 || result.Details.Field != "items" {
		t.Fatalf("批次数量错误响应不符: %d %s", response.Code, response.Body.String())
	}
}

// TestImportOnlyPrincipalReadsCapabilitiesWithoutOrdinaryAccess 验证导入专用主体可查能力且不获得普通合同读取权。
// 参数：t为测试上下文，身份为显式测试主体，不替代真实SSO。
// 返回值：无；导入入口被拒或普通读取被放宽时失败。
func TestImportOnlyPrincipalReadsCapabilitiesWithoutOrdinaryAccess(t *testing.T) {
	for _, permission := range []string{"contract.import.preview", "contract.import.commit", "contract.import.approve", "contract.import.release"} {
		t.Run(permission, func(t *testing.T) {
			actor := application.Principal{TenantID: "tenant", UserID: "import-user", Permissions: map[string]bool{permission: true}, PermissionScopes: map[string]contract.ScopeFilter{permission: {AllowSelf: true}}}
			router := NewRouter(&application.Service{ExternalPDFEnabled: true, ContractImportEnabled: true}, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-source-capabilities", nil))
			var body struct {
				Data struct {
					PDFEnabled   bool `json:"pdf_enabled"`
					BatchEnabled bool `json:"batch_enabled"`
				} `json:"data"`
			}
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || !body.Data.PDFEnabled || !body.Data.BatchEnabled {
				t.Fatalf("import-only capability denied: %d %s", response.Code, response.Body.String())
			}
			response = httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil))
			if response.Code != 403 {
				t.Fatalf("ordinary read was expanded: %d", response.Code)
			}
		})
	}
}

// TestImportCapabilitiesRejectsUnrelatedOrUnscopedPrincipal 验证无关权限和不可用范围不能读取导入能力。
// 参数：t为测试上下文，覆盖无权限及NONE范围。
// 返回值：无；缺少实际能力仍成功时失败。
func TestImportCapabilitiesRejectsUnrelatedOrUnscopedPrincipal(t *testing.T) {
	for _, permission := range []string{"approval.view", "contract.import.approve"} {
		actor := application.Principal{TenantID: "tenant", UserID: "user", Permissions: map[string]bool{permission: true}, PermissionScopes: map[string]contract.ScopeFilter{permission: {}}}
		router := NewRouter(&application.Service{}, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-source-capabilities", nil))
		if response.Code != 403 {
			t.Fatalf("unscoped capability accepted: %d", response.Code)
		}
	}
}

type importBatchReadStore struct {
	*importPreviewPermissionStore
	allQuery bool
}

// ListImportBatchesPage 返回候选批次并记录分页前的全范围条件，交实际Service复核。
// 参数：ctx/tenant/actor为可信身份，all为授权范围，limit/offset为分页。
// 返回值：显式候选批次，不代替真实MySQL筛选测试。
func (r *importBatchReadStore) ListImportBatchesPage(ctx context.Context, tenant, actor string, all bool, limit, offset int) ([]contract.ImportBatch, error) {
	r.allQuery = all
	return []contract.ImportBatch{r.batch}, nil
}

// TestImportOperationPermissionsCanReadTheirScopedBatch 验证提交和放行主体能读取对应批次，读取不会授予预检写入权。
// 参数：t为HTTP与应用授权测试上下文，候选仓储为显式替身。
// 返回值：无；范围、受派权限或分页全范围条件不正确时失败。
func TestImportOperationPermissionsCanReadTheirScopedBatch(t *testing.T) {
	for _, tc := range []struct {
		name, permission, creator string
		all, allow                bool
	}{
		{"commit own", "contract.import.commit", "operator", false, true},
		{"commit foreign", "contract.import.commit", "another", false, false},
		{"release all", "contract.import.release", "another", true, true},
		{"release own", "contract.import.release", "operator", false, true},
		{"release foreign", "contract.import.release", "another", false, false},
		{"approve unassigned", "contract.import.approve", "another", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actor := application.Principal{TenantID: "tenant", UserID: "operator", Permissions: map[string]bool{tc.permission: true}, PermissionScopes: map[string]contract.ScopeFilter{tc.permission: {AllowAll: tc.all, AllowSelf: !tc.all}}}
			repo := &importBatchReadStore{importPreviewPermissionStore: &importPreviewPermissionStore{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: tc.creator, Version: 1, State: "PREVIEWED"}}}
			router := NewRouter(&application.Service{Repo: repo}, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-import-batches/batch", nil))
			expected := 403
			if tc.allow {
				expected = 200
			}
			if response.Code != expected {
				t.Fatalf("scoped read=%d expected=%d", response.Code, expected)
			}
			response = httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-import-batches?limit=1", nil))
			if response.Code != 200 {
				t.Fatalf("list denied: %d %s", response.Code, response.Body.String())
			}
			var body struct {
				Data struct {
					Items []contract.ImportBatch `json:"items"`
				} `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if (len(body.Data.Items) == 1) != tc.allow || repo.allQuery != (tc.all && tc.permission != "contract.import.approve") || repo.writes != 0 {
				t.Fatalf("scope/list mismatch items=%d all=%v writes=%d", len(body.Data.Items), repo.allQuery, repo.writes)
			}
		})
	}
}

// TestImportReadOperationDoesNotGrantPreviewWrites 验证可读取操作批次的身份仍不得重置预检。
// 参数：t为测试上下文，覆盖全范围提交和放行但缺预检权限的主体。
// 返回值：无；预检非403或批次被写入时失败。
func TestImportReadOperationDoesNotGrantPreviewWrites(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com")
	for _, permission := range []string{"contract.import.commit", "contract.import.release"} {
		actor := application.Principal{TenantID: "tenant", UserID: "operator", Permissions: map[string]bool{permission: true}, PermissionScopes: map[string]contract.ScopeFilter{permission: {AllowAll: true}}}
		repo := &importPreviewPermissionStore{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant", ActorID: "operator", Revision: 1, Version: 7, State: "PREVIEWED"}}
		router := NewRouter(&application.Service{Repo: repo, ContractImportEnabled: true}, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
		request := httptest.NewRequest(http.MethodPost, "http://example.com/api/v1/contract-import-batches/batch/preview", strings.NewReader(`{"revision":1}`))
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != 403 || repo.writes != 0 || repo.batch.Version != 7 {
			t.Fatalf("read expanded to preview write for %s: status=%d writes=%d", permission, response.Code, repo.writes)
		}
	}
}

// TestImportListCursorSurvivesAuthorizationFiltering 验证过滤后的空页仍保留候选分页游标。
// 参数：t为HTTP测试上下文，候选页有一条当前SELF主体不可见的旧受派记录。
// 返回值：无；将授权过滤后的行数误当候选终点时失败。
func TestImportListCursorSurvivesAuthorizationFiltering(t *testing.T) {
	actor := application.Principal{TenantID: "tenant", UserID: "operator", Permissions: map[string]bool{"contract.import.commit": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.import.commit": {AllowSelf: true}}}
	repo := &importBatchReadStore{importPreviewPermissionStore: &importPreviewPermissionStore{batch: contract.ImportBatch{ID: "not-visible", TenantID: "tenant", ActorID: "another", Version: 1, State: "PREVIEWED"}}}
	router := NewRouter(&application.Service{Repo: repo}, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-import-batches?limit=1&offset=2", nil))
	var body struct {
		Data struct {
			Items      []contract.ImportBatch `json:"items"`
			HasMore    bool                   `json:"has_more"`
			NextOffset *int                   `json:"next_offset"`
		} `json:"data"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || len(body.Data.Items) != 0 || !body.Data.HasMore || body.Data.NextOffset == nil || *body.Data.NextOffset != 3 {
		t.Fatalf("filtered page lost its candidate cursor: %d %s", response.Code, response.Body.String())
	}
}
