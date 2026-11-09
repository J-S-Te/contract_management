package contract

import "testing"

// TestHistoricalClosedWithoutFinance 验证历史结清可只导档案，不冒认结算对账完成。
// 参数：t为领域契约测试上下文。
// 返回值：无；NONE策略或独立来源声明被拒绝时失败。
func TestHistoricalClosedWithoutFinance(t *testing.T) {
	item := ImportItem{Mode: ImportHistoricalClosed,
		Policy: &ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE",
			ReportMode: "HISTORICAL_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"},
		HistoryEvidence: &ImportHistoryEvidence{HistoricalStatus: "completed",
			TargetOwnerUserID: "actor", ReportRequiredChannels: "API_ONLY"}}
	if err := ValidateHistoryEvidence(item); err != nil {
		t.Fatalf("历史结清不参与财务时不应要求财务事实或分期: %v", err)
	}
	for _, finance := range []string{"EVIDENCE_ONLY", "FULL_REPLAY"} {
		item.Policy.FinanceMode = finance
		if err := ValidateImportPolicy(item.Mode, item.Policy); err != nil {
			t.Fatalf("旧策略%s必须保留读取兼容: %v", finance, err)
		}
	}
}
