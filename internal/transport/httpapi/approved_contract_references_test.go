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

// approvedContractReferenceRepository 记录项目集成数据流实际收到的关键字/游标。
type approvedContractReferenceRepository struct {
	application.Repository
	keyword string
	afterID string
	limit   int
	called  bool
	items   []contract.Contract
}

func (r *approvedContractReferenceRepository) ListApprovedContractReferences(_ context.Context, _, keyword, afterID string, limit int) ([]contract.Contract, error) {
	r.called = true
	r.keyword, r.afterID, r.limit = keyword, afterID, limit
	return r.items, nil
}

func newApprovedContractReferenceContext(t *testing.T, permissions map[string]bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/project/approved-contract-references", nil)
	c.Set(principalKey, application.Principal{
		TenantID: "tenant-1", Permissions: permissions,
		PermissionScopes: map[string]contract.ScopeFilter{"contract.approved.read": {AllowAll: true}},
	})
	return c, recorder
}

// TestProjectApprovedContractReferencesForwardKeywordAndExposePickerFields 固定 BIZ-XAPI-09
// 的合同侧契约：keyword 去空白后透传，响应必须带 title/customer_name/status/approval_passed，
// 否则项目侧选择器无法跨 200 条窗口展示与过滤。
func TestProjectApprovedContractReferencesForwardKeywordAndExposePickerFields(t *testing.T) {
	repository := &approvedContractReferenceRepository{items: []contract.Contract{{
		ID: "C-1", Number: "HT-1", Title: "等保测评服务", CustomerName: "示例客户",
		Version: 3, Status: contract.StatusActive,
	}}}
	handler := &Handler{service: &application.Service{Repo: repository}}
	c, recorder := newApprovedContractReferenceContext(t, map[string]bool{"contract.approved.read": true})
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/project/approved-contract-references?keyword=%20%E7%AD%89%E4%BF%9D%20&after_id=C-0&limit=20", nil)
	handler.listProjectApprovedContractReferences(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !repository.called || repository.keyword != "等保" || repository.afterID != "C-0" || repository.limit != 20 {
		t.Fatalf("keyword=%q after=%q limit=%d", repository.keyword, repository.afterID, repository.limit)
	}
	var body struct {
		Data struct {
			Contracts []map[string]any `json:"contracts"`
			NextAfter string           `json:"next_after_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data.Contracts) != 1 {
		t.Fatalf("contracts=%v", body.Data.Contracts)
	}
	item := body.Data.Contracts[0]
	if item["title"] != "等保测评服务" || item["customer_name"] != "示例客户" || item["status"] != string(contract.StatusActive) || item["approval_passed"] != true {
		t.Fatalf("引用响应缺少选择器字段: %v", item)
	}
	// 页未满时 next_after_id 必须为空，避免前端无谓地再翻一页。
	if body.Data.NextAfter != "" {
		t.Fatalf("next_after_id=%q，want 空", body.Data.NextAfter)
	}
}

// TestProjectApprovedContractReferencesRejectsOversizedKeyword 固定关键字长度上限：
// 超过 100 字符直接 422，不能把脏输入转成合同依赖不可用。
func TestProjectApprovedContractReferencesRejectsOversizedKeyword(t *testing.T) {
	repository := &approvedContractReferenceRepository{}
	handler := &Handler{service: &application.Service{Repo: repository}}
	c, recorder := newApprovedContractReferenceContext(t, map[string]bool{"contract.approved.read": true})
	long := strings.Repeat("字", 101)
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/project/approved-contract-references?keyword="+long, nil)
	handler.listProjectApprovedContractReferences(c)

	if recorder.Code != http.StatusUnprocessableEntity || !strings.Contains(recorder.Body.String(), "CON_VALIDATION_ERROR") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.called {
		t.Fatal("超长关键字不应触达仓储")
	}
}

// TestProjectApprovedContractReferencesRequireApprovedReadPermission 保留原权限边界：
// 缺少 contract.approved.read 时 403，不能因为新增 keyword 参数放宽。
func TestProjectApprovedContractReferencesRequireApprovedReadPermission(t *testing.T) {
	repository := &approvedContractReferenceRepository{}
	handler := &Handler{service: &application.Service{Repo: repository}}
	c, recorder := newApprovedContractReferenceContext(t, map[string]bool{})
	c.Request = httptest.NewRequest(http.MethodGet, "/internal/v1/project/approved-contract-references?keyword=HT", nil)
	handler.listProjectApprovedContractReferences(c)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.called {
		t.Fatal("无权限请求不应触达仓储")
	}
}
