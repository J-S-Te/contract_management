package mysql

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type importProvisionNumberPreparer interface {
	PrepareImportContractNumber(context.Context, string, string, uint64, uint64, string, string) (string, error)
}

// TestPrepareImportContractNumberReservesWithoutConfirmation 验证验真前预留编号与确认门禁分离。
// 参数：t为测试上下文；仅使用显式隔离合同测试库。
// 返回值：预留触发归档、放行目标、不幂等或接受失效围栏时失败。
func TestPrepareImportContractNumberReservesWithoutConfirmation(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	b := autoApproveHistoryFixture(t, r, tenant)
	preparer, ok := any(r).(importProvisionNumberPreparer)
	if !ok {
		t.Fatal("缺少历史开立验真前的受控编号预留能力")
	}
	item := b.Items[0]
	ctx := context.Background()
	if _, err := preparer.PrepareImportContractNumber(ctx, tenant, b.ID, b.Revision, b.Epoch+1, item.ID, item.ContractID); !errors.Is(err, application.ErrImportConflict) {
		t.Fatalf("失效围栏=%v", err)
	}
	number, err := preparer.PrepareImportContractNumber(ctx, tenant, b.ID, b.Revision, b.Epoch, item.ID, item.ContractID)
	if err != nil || number == "" {
		t.Fatalf("预留编号=%q err=%v", number, err)
	}
	c, err := r.GetContract(ctx, tenant, item.ContractID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := r.GetImportBatch(ctx, tenant, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || c.Status != contract.StatusDraft || stored.Items[0].Confirmed || len(stored.Items[0].Targets) != 0 {
		t.Fatalf("预留提前确认: version=%d status=%s item=%+v", c.Version, c.Status, stored.Items[0])
	}
	summary, summaryErr := (&application.Service{Repo: r}).ContractSummary(ctx, tenant, item.ContractID, 42)
	if summaryErr != nil || summary.ContractNumber != number || summary.ContractID != item.ContractID {
		t.Fatalf("预留后真实摘要验真失败: summary=%+v err=%v", summary, summaryErr)
	}
	repeated, err := preparer.PrepareImportContractNumber(ctx, tenant, b.ID, b.Revision, b.Epoch, item.ID, item.ContractID)
	if err != nil || repeated != number {
		t.Fatalf("重复预留=%q err=%v", repeated, err)
	}
	if err = r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error { current.State = "PAUSED"; current.Epoch++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, item.ID, []string{"admin"}, contractAutoPolicy(b, 1)); !errors.Is(err, application.ErrImportConflict) {
		t.Fatalf("暂停后旧签约代次仍可确认: %v", err)
	}
	if err = r.UpdateImportBatch(ctx, tenant, b.ID, func(current *contract.ImportBatch) error { current.State = "RUNNING"; return nil }); err != nil {
		t.Fatal(err)
	}
	b, err = r.GetImportBatch(ctx, tenant, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.AutoConfirmHistoricalImportItem(ctx, tenant, b.ID, item.ID, []string{"admin"}, contractAutoPolicy(b, 1)); err != nil {
		t.Fatal(err)
	}
	c, err = r.GetContract(ctx, tenant, item.ContractID)
	if err != nil || c.Number != number || c.Version != 2 {
		t.Fatalf("确认未复用编号: c=%+v err=%v", c, err)
	}
}
