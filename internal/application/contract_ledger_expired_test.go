package application

import (
	"context"
	"testing"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ledgerContractAt 构造一份到期日相对北京今天偏移若干天的合同。
func ledgerContractAt(id string, status contract.Status, offsetDays int) contract.Contract {
	today := businessclock.Today()
	due := today.AddDate(0, 0, offsetDays)
	return contract.Contract{ID: id, TenantID: "tenant-1", Status: status, EndDate: &due}
}

// ledgerIDs 只取合同 ID，避免失败信息被整份合同结构淹没。
func ledgerIDs(items []contract.Contract) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestListContractsPageForwardsExpiredFilter 锁定服务层下推：
// expired 去两端空白后原样进入仓储查询（解析与谓词由仓储/内存路径统一完成）。
func TestListContractsPageForwardsExpiredFilter(t *testing.T) {
	repository := &ledgerPageRepository{total: 2}
	service := &Service{Repo: repository}

	if _, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Expired: " TRUE ", Offset: 0, Limit: 20,
	}); err != nil {
		t.Fatalf("ListContractsPage() error = %v", err)
	}
	if repository.query.Expired != "TRUE" {
		t.Fatalf("expired 下推 = %q，want %q（去两端空白后原样下推）", repository.query.Expired, "TRUE")
	}
}

// TestListContractsPageFallsBackWithExpiredFilter 锁定兼容回退路径的过期筛选：
// 仓储只有旧 ListContracts 时，expired=true 只保留“参与过期判定的状态 + 到期日早于
// 北京今天”的合同；expired=false 是其否定（不是 status != active），到期日为空视为未过期。
func TestListContractsPageFallsBackWithExpiredFilter(t *testing.T) {
	draftPast := ledgerContractAt("draft-past", contract.StatusDraft, -1)
	archivedPast := ledgerContractAt("archived-past", contract.StatusArchived, -1)
	noEndDate := contract.Contract{ID: "no-end-date", TenantID: "tenant-1", Status: contract.StatusActive}
	repository := &ledgerLegacyRepository{items: []contract.Contract{
		ledgerContractAt("active-expired", contract.StatusActive, -1),
		ledgerContractAt("progress-expired", contract.StatusInProgress, -1),
		ledgerContractAt("active-due-today", contract.StatusActive, 0),
		ledgerContractAt("pending-pay-future", contract.StatusPendingPay, 1),
		draftPast, archivedPast, noEndDate,
	}}
	service := &Service{Repo: repository}

	page, err := service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Expired: "true", Offset: 0, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListContractsPage(expired=true) error = %v", err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("expired=true 结果 = %v（total=%d），want 仅 active-expired/progress-expired", ledgerIDs(page.Items), page.Total)
	}
	if page.Items[0].ID != "active-expired" || page.Items[1].ID != "progress-expired" {
		t.Fatalf("expired=true 行 = %#v，want active-expired, progress-expired", page.Items)
	}

	page, err = service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Expired: "false", Offset: 0, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListContractsPage(expired=false) error = %v", err)
	}
	// 否定口径：草稿/归档的到期日虽已过去，但不在过期判定范围内；到期日为空也不算过期。
	if page.Total != 5 || len(page.Items) != 5 {
		t.Fatalf("expired=false 结果 = %v（total=%d），want 5 条未过期", ledgerIDs(page.Items), page.Total)
	}
	got := make(map[string]bool, len(page.Items))
	for _, item := range page.Items {
		got[item.ID] = true
	}
	for _, id := range []string{"active-due-today", "pending-pay-future", "draft-past", "archived-past", "no-end-date"} {
		if !got[id] {
			t.Fatalf("expired=false 缺少 %q: %#v", id, got)
		}
	}
	if got["active-expired"] || got["progress-expired"] {
		t.Fatalf("expired=false 不应包含已过期合同: %#v", got)
	}

	// 非法取值退化为不过滤：全部行都返回，且不报错。
	page, err = service.ListContractsPage(context.Background(), ledgerActor(), contract.ContractListQuery{
		Expired: "yes", Offset: 0, Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListContractsPage(expired=yes) error = %v", err)
	}
	if page.Total != 7 {
		t.Fatalf("非法 expired 应不过滤，total = %d，want 7", page.Total)
	}
}
