package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ledgerPageRepository 实现可选能力接口 ContractPageRepository：
// 用来断言服务层把 keyword/status/type/offset/limit 原样下推，并透传 total。
type ledgerPageRepository struct {
	Repository
	filter contract.ScopeFilter
	query  contract.ContractListQuery
	items  []contract.Contract
	total  int64
}

func (r *ledgerPageRepository) ListContractsPageScoped(_ context.Context, filter contract.ScopeFilter, query contract.ContractListQuery) (contract.ContractPage, error) {
	r.filter, r.query = filter, query
	return contract.ContractPage{Items: r.items, Total: r.total}, nil
}

// ledgerLegacyRepository 只实现旧 Repository.ListContracts（没有分页能力），
// 用来锁定服务层回退路径：类型筛选和分页必须在内存里补齐。
type ledgerLegacyRepository struct {
	Repository
	tenantID, keyword, ownerUserID, status string
	limit                                  int
	items                                  []contract.Contract
}

func (r *ledgerLegacyRepository) ListContracts(_ context.Context, tenantID, keyword, ownerUserID, status string, limit int) ([]contract.Contract, error) {
	r.tenantID, r.keyword, r.ownerUserID, r.status, r.limit = tenantID, keyword, ownerUserID, status, limit
	return r.items, nil
}

func ledgerActor() Principal {
	return Principal{
		TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1",
		Permissions:      map[string]bool{"contract.read": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}},
	}
}

// TestListContractsPageForwardsKeywordStatusTypeAndTotal 锁定服务层下推：
// keyword/status/type 必须去首尾空格后原样出现在仓储查询里，total 来自仓储计数。
func TestListContractsPageForwardsKeywordStatusTypeAndTotal(t *testing.T) {
	repository := &ledgerPageRepository{total: 41, items: []contract.Contract{{ID: "c-21"}}}
	service := &Service{Repo: repository}

	page, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Keyword: " 等保 ", Status: " active ", Type: " 直签 ", Offset: 20, Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListContractsPage() error = %v", err)
	}
	if repository.query.Keyword != "等保" || repository.query.Status != "active" || repository.query.Type != "直签" {
		t.Fatalf("筛选下推 = %#v，want keyword/status/type 去空格后原样下推", repository.query)
	}
	if repository.query.Offset != 20 || repository.query.Limit != 20 {
		t.Fatalf("分页下推 = %#v，want offset=20 limit=20", repository.query)
	}
	if repository.filter.TenantID != "tenant-1" || !repository.filter.AllowAll {
		t.Fatalf("数据范围丢失 = %#v", repository.filter)
	}
	if page.Total != 41 || page.Page != 2 || page.PageSize != 20 || len(page.Items) != 1 {
		t.Fatalf("分页信封 = %#v，want total=41 page=2 page_size=20 items=1", page)
	}
}

// TestListContractsPageClampsPagingBounds 锁定越界钳制口径（不返回 422）：
// limit<=0 或 >200 回退到 50（与旧 ListContracts 一致），负 offset 归零。
func TestListContractsPageClampsPagingBounds(t *testing.T) {
	cases := []struct {
		name                  string
		offset, limit         int
		wantOffset, wantLimit int
		wantPage              int
	}{
		{name: "零值", offset: 0, limit: 0, wantOffset: 0, wantLimit: contractLedgerDefaultLimit, wantPage: 1},
		{name: "负 offset", offset: -7, limit: 20, wantOffset: 0, wantLimit: 20, wantPage: 1},
		{name: "超过上限", offset: 100, limit: 5000, wantOffset: 100, wantLimit: contractLedgerDefaultLimit, wantPage: 3},
	}
	for _, testCase := range cases {
		repository := &ledgerPageRepository{}
		service := &Service{Repo: repository}
		page, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
			Offset: testCase.offset, Limit: testCase.limit,
		})
		if err != nil {
			t.Fatalf("%s: ListContractsPage() error = %v", testCase.name, err)
		}
		if repository.query.Offset != testCase.wantOffset || repository.query.Limit != testCase.wantLimit {
			t.Fatalf("%s: 钳制后 = offset %d limit %d，want offset %d limit %d",
				testCase.name, repository.query.Offset, repository.query.Limit, testCase.wantOffset, testCase.wantLimit)
		}
		if page.Page != testCase.wantPage {
			t.Fatalf("%s: page = %d，want %d", testCase.name, page.Page, testCase.wantPage)
		}
		if page.Items == nil {
			t.Fatalf("%s: items 必须是空数组而不是 nil（JSON null）", testCase.name)
		}
	}
}

// TestListContractsPageFallsBackWithTypeFilterAndPaging 锁定兼容回退：
// 仓储只有旧 ListContracts 时，类型筛选在内存里完成，分页窗口与 total 与筛选结果一致。
func TestListContractsPageFallsBackWithTypeFilterAndPaging(t *testing.T) {
	repository := &ledgerLegacyRepository{items: []contract.Contract{
		{ID: "c-1", TenantID: "tenant-1", Type: "直签"}, {ID: "c-2", TenantID: "tenant-1", Type: "三方"},
		{ID: "c-3", TenantID: "tenant-1", Type: "三方"}, {ID: "c-4", TenantID: "tenant-1", Type: "直签"},
	}}
	service := &Service{Repo: repository}

	page, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Keyword: "HT", Status: "active", Type: "三方", Offset: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("ListContractsPage() error = %v", err)
	}
	if repository.keyword != "HT" || repository.status != "active" {
		t.Fatalf("回退路径没有下推 keyword/status: %#v", repository)
	}
	if page.Total != 2 || len(page.Items) != 1 || page.Items[0].ID != "c-3" {
		t.Fatalf("回退分页 = %#v，want total=2 第二页第 1 行 c-3", page)
	}
	if page.Page != 2 || page.PageSize != 1 {
		t.Fatalf("回退页码 = %d/%d，want 2/1", page.Page, page.PageSize)
	}
}

// TestListContractsLegacyEntryKeepsBareSliceAndOldLimitDefault 锁定旧入口语义：
// 不带分页参数时仍是第一页、limit 越界回退 50，调用方拿到的还是切片。
func TestListContractsLegacyEntryKeepsBareSliceAndOldLimitDefault(t *testing.T) {
	repository := &ledgerLegacyRepository{items: []contract.Contract{{ID: "c-1", TenantID: "tenant-1", Type: "直签"}}}
	service := &Service{Repo: repository}

	items, err := service.ListContracts(context.Background(), ledgerActor(), "HT-1", "active", 0)
	if err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.limit != contractLedgerDefaultLimit {
		t.Fatalf("旧入口 limit = %d，want %d", repository.limit, contractLedgerDefaultLimit)
	}
	if len(items) != 1 || items[0].ID != "c-1" {
		t.Fatalf("旧入口返回 = %#v，want 单条合同", items)
	}
}

// TestListContractsPageWithoutReadPermissionIsForbidden 锁定权限：
// 没有 contract.read 时不分页、不查库，直接 Forbidden。
func TestListContractsPageWithoutReadPermissionIsForbidden(t *testing.T) {
	repository := &ledgerPageRepository{}
	service := &Service{Repo: repository}
	actor := Principal{TenantID: "tenant-1", UserID: "user-1"}

	if _, err := service.ListContractsPage(context.Background(), actor, contract.ContractListQuery{Limit: 20}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ListContractsPage() error = %v，want ErrForbidden", err)
	}
	if repository.filter.TenantID != "" {
		t.Fatalf("无权限时不应触达仓储: %#v", repository)
	}
}
