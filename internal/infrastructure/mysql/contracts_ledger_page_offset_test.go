package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ledgerPageFilter 是分页台账用例共用的数据范围（租户 + 本人责任范围）。
func ledgerPageFilter() contract.ScopeFilter {
	return contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
}

// TestListContractsPageAfterCountClampsOverRangeOffset 固定深页请求的资源边界：
// COUNT(*) 拿到真实 total=45 之后，取页语句必须绑定收敛到 total 以内的 OFFSET。
// 反例（修复前）：page=90000&page_size=200 → OFFSET 17,999,800，MySQL 得先扫完过滤集
// 再把 1,800 万行全部丢弃才返回空页，单请求即可消耗数秒 CPU/IO。
// DryRun 下 COUNT 不执行、total 恒为 0，所以这里直接驱动「计数之后」的取页步骤，
// 把 total 作为入参固定下来；COUNT 与取页仍由 ListContractsPageScoped 串起来。
func TestListContractsPageAfterCountClampsOverRangeOffset(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	const realTotal = 45
	page, err := repository.listContractsPageAfterCount(context.Background(), ledgerPageFilter(), contract.ContractListQuery{
		Offset: 17999800, Limit: 200,
	}, realTotal)
	if err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	if !strings.Contains(recorder.sql, "LIMIT ? OFFSET ?") {
		t.Fatalf("取页语句缺少绑定化的 LIMIT/OFFSET: %s", recorder.sql)
	}
	if containsParam(recorder.params, 17999800) {
		t.Fatalf("OFFSET 仍绑定请求的深页值 17,999,800: %#v", recorder.params)
	}
	if !containsParam(recorder.params, realTotal) {
		t.Fatalf("OFFSET 未收敛到 total=%d: %#v", realTotal, recorder.params)
	}
	// 超出末尾：空 items + 真实 total + 收敛后的页码（45 行、page_size=200 时只有第 1 页）。
	if page.Total != realTotal || page.Page != 1 || page.PageSize != 200 || len(page.Items) != 0 {
		t.Fatalf("信封 = total %d page %d page_size %d items %d，want 45/1/200/0",
			page.Total, page.Page, page.PageSize, len(page.Items))
	}
	// 数据范围谓词不能因为收敛 offset 而丢失。
	for _, fragment := range []string{"tenant_id = ?", "owner_identity_id = ?", "ORDER BY updated_at DESC, id DESC"} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("取页语句缺少 %q: %s", fragment, recorder.sql)
		}
	}
}

// TestListContractsPageAfterCountKeepsInRangeOffset 固定回归：请求页在范围内时
// OFFSET 与页码逐字节不变（offset=40、limit=20、total=45 → 第 3 页）。
func TestListContractsPageAfterCountKeepsInRangeOffset(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	page, err := repository.listContractsPageAfterCount(context.Background(), ledgerPageFilter(), contract.ContractListQuery{
		Offset: 40, Limit: 20,
	}, 45)
	if err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	if !containsParam(recorder.params, 40) {
		t.Fatalf("范围内 OFFSET 未原样绑定 40: %#v", recorder.params)
	}
	if containsParam(recorder.params, 45) {
		t.Fatalf("范围内 OFFSET 不应被改写成 total: %#v", recorder.params)
	}
	if page.Total != 45 || page.Page != 3 || page.PageSize != 20 {
		t.Fatalf("信封 = total %d page %d page_size %d，want 45/3/20", page.Total, page.Page, page.PageSize)
	}
}

// TestListContractsPageAfterCountKeepsEmptyLedgerOffset 固定 total=0 的边界：
// 过滤集为空时没有可丢弃的行，OFFSET 不产生额外扫描，保持请求值（信封也保持原页码），
// 不用「第 1 页」覆盖空台账的请求页。
func TestListContractsPageAfterCountKeepsEmptyLedgerOffset(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	page, err := repository.listContractsPageAfterCount(context.Background(), ledgerPageFilter(), contract.ContractListQuery{
		Offset: 40, Limit: 20,
	}, 0)
	if err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	if !containsParam(recorder.params, 40) {
		t.Fatalf("空过滤集不应改写 OFFSET: %#v", recorder.params)
	}
	if page.Total != 0 || page.Page != 3 || len(page.Items) != 0 {
		t.Fatalf("信封 = total %d page %d items %d，want 0/3/0", page.Total, page.Page, len(page.Items))
	}
}
