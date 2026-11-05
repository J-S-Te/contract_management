package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ledgerPageFake 实现 application.ContractPageRepository：记录服务层下推的查询，
// 返回预置的分页结果，用来锁定 HTTP 协议形状（裸数组 / 分页信封）。
type ledgerPageFake struct {
	application.Repository
	filter contract.ScopeFilter
	query  contract.ContractListQuery
	items  []contract.Contract
	total  int64
}

func (r *ledgerPageFake) ListContractsPageScoped(_ context.Context, filter contract.ScopeFilter, query contract.ContractListQuery) (contract.ContractPage, error) {
	r.filter, r.query = filter, query
	return contract.ContractPage{Items: r.items, Total: r.total}, nil
}

// ledgerRequest 构造带合同读取权限主体的台账请求上下文。
func ledgerRequest(t *testing.T, target string) (*gin.Context, *httptest.ResponseRecorder, *ledgerPageFake) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repository := &ledgerPageFake{total: 41, items: []contract.Contract{{ID: "c-21", Title: "隔离测试"}}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("GET", target, nil)
	c.Set(principalKey, application.Principal{
		TenantID: "tenant-1", UserID: "user-1",
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}},
	})
	return c, recorder, repository
}

// ledgerEnvelope 是统一响应信封中台账数据的解码结果。
type ledgerEnvelope struct {
	Code string          `json:"code"`
	Data json.RawMessage `json:"data"`
}

func decodeLedgerEnvelope(t *testing.T, body []byte) ledgerEnvelope {
	t.Helper()
	var envelope ledgerEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v; body = %s", err, body)
	}
	return envelope
}

// TestListContractsLegacyRequestKeepsBareArray 锁定向后兼容：不带 page/page_size 时
// 响应 data 仍是数组（内置 web 页把 body.data 当数组渲染），limit 语义不变。
func TestListContractsLegacyRequestKeepsBareArray(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts?limit=200&keyword=HT&status=active")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	if recorder.Code != 200 {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	envelope := decodeLedgerEnvelope(t, recorder.Body.Bytes())
	if !strings.HasPrefix(strings.TrimSpace(string(envelope.Data)), "[") {
		t.Fatalf("旧协议 data 必须是裸数组: %s", envelope.Data)
	}
	if repository.query.Limit != 200 || repository.query.Keyword != "HT" || repository.query.Status != "active" {
		t.Fatalf("旧协议下推 = %#v，want limit=200 keyword=HT status=active", repository.query)
	}
	if repository.query.Offset != 0 {
		t.Fatalf("旧协议 offset = %d，want 0", repository.query.Offset)
	}
}

// TestListContractsPaginatedRequestReturnsEnvelope 锁定分页信封：
// items/total/page/page_size 齐全，type 作为服务端筛选下推。
func TestListContractsPaginatedRequestReturnsEnvelope(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts?page=2&page_size=20&status=active&type=%E7%9B%B4%E7%AD%BE&keyword=HT")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	if recorder.Code != 200 {
		t.Fatalf("status = %d; body = %s", recorder.Code, recorder.Body.String())
	}
	var page struct {
		Items    []contract.Contract `json:"items"`
		Total    int64               `json:"total"`
		Page     int                 `json:"page"`
		PageSize int                 `json:"page_size"`
	}
	if err := json.Unmarshal(decodeLedgerEnvelope(t, recorder.Body.Bytes()).Data, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.Total != 41 || page.Page != 2 || page.PageSize != 20 || len(page.Items) != 1 {
		t.Fatalf("分页信封 = %#v，want total=41 page=2 page_size=20 items=1", page)
	}
	if repository.query.Type != "直签" || repository.query.Status != "active" || repository.query.Keyword != "HT" {
		t.Fatalf("筛选下推 = %#v，want type=直签 status=active keyword=HT", repository.query)
	}
	if repository.query.Offset != 20 || repository.query.Limit != 20 {
		t.Fatalf("分页下推 = %#v，want offset=20 limit=20", repository.query)
	}
}

// TestListContractsPageParamsAreClamped 锁定越界钳制而不是 422：
// page<1 → 1，超大 page → 100000（避免 offset 溢出），page_size 越界 → 20。
// 信封里的 page 是服务端按真实 total 收敛后的页码（"极大页码" 用例中 total=41、
// page_size=200 时整份台账只有 1 页，所以回包 page=1），但下推给仓储的 offset
// 仍是钳制后的 19,999,800——收敛发生在仓储拿到 COUNT 之后。
func TestListContractsPageParamsAreClamped(t *testing.T) {
	cases := []struct {
		name                   string
		target                 string
		wantOffset, wantLimit  int
		wantPage, wantPageSize int
	}{
		{name: "page=0 与超大 page_size", target: "/api/v1/contracts?page=0&page_size=1000", wantOffset: 0, wantLimit: 20, wantPage: 1, wantPageSize: 20},
		{name: "只带 page", target: "/api/v1/contracts?page=3", wantOffset: 40, wantLimit: 20, wantPage: 3, wantPageSize: 20},
		{name: "只带 page_size", target: "/api/v1/contracts?page_size=50", wantOffset: 0, wantLimit: 50, wantPage: 1, wantPageSize: 50},
		{name: "非数字", target: "/api/v1/contracts?page=abc&page_size=xyz", wantOffset: 0, wantLimit: 20, wantPage: 1, wantPageSize: 20},
		{name: "极大页码", target: "/api/v1/contracts?page=999999999&page_size=200", wantOffset: 19999800, wantLimit: 200, wantPage: 1, wantPageSize: 200},
	}
	for _, testCase := range cases {
		c, recorder, repository := ledgerRequest(t, testCase.target)
		h := &Handler{service: &application.Service{Repo: repository}}
		h.listContracts(c)
		if recorder.Code != 200 {
			t.Fatalf("%s: status = %d; body = %s", testCase.name, recorder.Code, recorder.Body.String())
		}
		var page struct {
			Page     int `json:"page"`
			PageSize int `json:"page_size"`
		}
		if err := json.Unmarshal(decodeLedgerEnvelope(t, recorder.Body.Bytes()).Data, &page); err != nil {
			t.Fatalf("%s: decode page: %v", testCase.name, err)
		}
		if page.Page != testCase.wantPage || page.PageSize != testCase.wantPageSize {
			t.Fatalf("%s: 信封 = page %d page_size %d，want %d/%d",
				testCase.name, page.Page, page.PageSize, testCase.wantPage, testCase.wantPageSize)
		}
		if repository.query.Offset != testCase.wantOffset || repository.query.Limit != testCase.wantLimit {
			t.Fatalf("%s: 下推 = offset %d limit %d，want %d/%d",
				testCase.name, repository.query.Offset, repository.query.Limit, testCase.wantOffset, testCase.wantLimit)
		}
	}
}

// TestListContractsPageResponseIsNotTruncatedByLimit 锁定无 total 的旧缺陷已修复：
// 分页响应必须带服务端计数，limit 只影响本页行数，不影响 total。
func TestListContractsPageResponseIsNotTruncatedByLimit(t *testing.T) {
	c, recorder, repository := ledgerRequest(t, "/api/v1/contracts?page=1&page_size=20&limit=5")
	h := &Handler{service: &application.Service{Repo: repository}}
	h.listContracts(c)

	var page struct {
		Total    int64 `json:"total"`
		PageSize int   `json:"page_size"`
	}
	if err := json.Unmarshal(decodeLedgerEnvelope(t, recorder.Body.Bytes()).Data, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if page.Total != 41 || page.PageSize != 20 {
		t.Fatalf("total/page_size = %d/%d，want 41/20（分页请求忽略旧 limit）", page.Total, page.PageSize)
	}
	if repository.query.Limit != 20 {
		t.Fatalf("分页请求 limit 下推 = %d，want 20", repository.query.Limit)
	}
}
