package application

import (
	"context"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestListContractsPageReportsClampedPageForOverRangeRequest 固定服务层信封口径：
// 请求 page=100000&page_size=200（offset 19,999,800）时，服务层在拿到真实 total 之前
// 无法收敛 offset，所以下推给仓储的 offset 仍是 19,999,800（生产仓储在 COUNT 之后收敛）；
// 但回包的 page 必须按 total 收敛，不能把请求页码原样回声给调用方。
func TestListContractsPageReportsClampedPageForOverRangeRequest(t *testing.T) {
	repository := &ledgerPageRepository{total: 41, items: []contract.Contract{{ID: "c-21"}}}
	service := &Service{Repo: repository}

	page, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Offset: 19999800, Limit: 200,
	})
	if err != nil {
		t.Fatalf("ListContractsPage() error = %v", err)
	}
	if repository.query.Offset != 19999800 || repository.query.Limit != 200 {
		t.Fatalf("下推 = offset %d limit %d，want 19999800/200（收敛发生在仓储 COUNT 之后）",
			repository.query.Offset, repository.query.Limit)
	}
	// 41 行、page_size=200 时整份台账只有第 1 页：page 必须是 1 而不是 100000。
	if page.Page != 1 || page.PageSize != 200 || page.Total != 41 {
		t.Fatalf("信封 = page %d page_size %d total %d，want 1/200/41", page.Page, page.PageSize, page.Total)
	}
}

// TestListContractsPageFallbackClampsOutOfRangeOffset 固定回退路径（仓储只实现旧
// ListContracts）的越界 offset：先把 offset 收敛到本窗口能看到的行数再切片，
// 不能带着 100000 这类越界 offset 去切，也不能把不存在的页码写进信封。
func TestListContractsPageFallbackClampsOutOfRangeOffset(t *testing.T) {
	repository := &ledgerLegacyRepository{items: []contract.Contract{
		{ID: "c-1", TenantID: "tenant-1"}, {ID: "c-2", TenantID: "tenant-1"},
		{ID: "c-3", TenantID: "tenant-1"}, {ID: "c-4", TenantID: "tenant-1"},
	}}
	service := &Service{Repo: repository}

	page, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Offset: 100000, Limit: 20,
	})
	if err != nil {
		t.Fatalf("ListContractsPage() error = %v", err)
	}
	// 取数窗口仍以仓储 200 行上限为界（旧语义不变）。
	if repository.limit != 200 {
		t.Fatalf("回退取数窗口 = %d，want 200", repository.limit)
	}
	if page.Total != 4 || len(page.Items) != 0 {
		t.Fatalf("回退分页 = total %d items %d，want 4/0（越界 offset 收敛到末尾后返回空页）",
			page.Total, len(page.Items))
	}
	// 4 行、page_size=20 时只有第 1 页：页码按收敛后的 offset 计算，不是请求的第 5001 页。
	if page.Page != 1 || page.PageSize != 20 {
		t.Fatalf("回退页码 = %d/%d，want 1/20", page.Page, page.PageSize)
	}
}
