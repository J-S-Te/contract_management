package mysql

import (
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestHistoryWithoutFinanceCompletesAfterReport 验证合同档案不等待结算，也不伪造财务回执。
// 参数：t为无数据库副作用的状态契约测试上下文。
// 返回值：无；财务被排入任务或报告完成仍阻塞批次时失败。
func TestHistoryWithoutFinanceCompletesAfterReport(t *testing.T) {
	for _, mode := range []contract.ImportMode{contract.ImportArchiveOnly, contract.ImportHistoricalClosed} {
		item := contract.ImportItem{Mode: mode, Policy: &contract.ImportPolicy{ProjectMode: "NONE", FinanceMode: "NONE"}}
		targets := initialImportTargets(item)
		for index := range targets {
			if targets[index].Target == "FINANCE" && (targets[index].State != "NOT_APPLICABLE" ||
				targets[index].CommandID != "" || targets[index].ReceiptID != "") {
				t.Fatal("不参与结算不能排入财务迁移或伪造回执")
			}
			if targets[index].Target == "REPORT" {
				targets[index].State = "COMPLETE"
			}
		}
		batch := contract.ImportBatch{Items: []contract.ImportItemState{{Targets: targets}}}
		updateImportTargetBatchState(&batch)
		if batch.State != "COMPLETED" {
			t.Fatalf("档案不应等待结算: %s", batch.State)
		}
	}
}
