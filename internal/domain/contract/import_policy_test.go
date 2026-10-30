package contract

import "testing"

// TestImportPolicyMatrix 验证历史模式不会获得不适用的派生能力。
// 参数：t 为策略矩阵测试上下文。
// 返回值：无；错误接受不安全组合时测试失败。
func TestImportPolicyMatrix(t *testing.T) {
	archive := ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE",
		ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
	live := ImportPolicy{Version: 1, ProjectMode: "LINK_EXISTING", FinanceMode: "FULL_REPLAY",
		ReportMode: "HISTORICAL_SEPARATE", ResponsibilityMode: "ACTIVATE_ON_RELEASE", SigningMode: "DECLARED_ONLY"}
	closed := ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "EVIDENCE_ONLY",
		ReportMode: "HISTORICAL_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
	tests := []struct {
		name   string
		mode   ImportMode
		policy *ImportPolicy
		valid  bool
	}{
		{"普通旧合同", "", nil, true}, {"普通批次", ImportNormal, nil, true},
		{"普通合同不能套历史策略", ImportNormal, &archive, false},
		{"档案", ImportArchiveOnly, &archive, true},
		{"履约接续", ImportContinueLive, &live, true},
		{"历史结清凭证", ImportHistoricalClosed, &closed, true},
		{"历史缺策略", ImportArchiveOnly, nil, false},
		{"未知模式", "UNRECOGNIZED", &archive, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateImportPolicy(tt.mode, tt.policy)
			if (err == nil) != tt.valid {
				t.Fatalf("mode=%q: error=%v", tt.mode, err)
			}
		})
	}
	archive.ProjectMode = "CREATE_NEW"
	if ValidateImportPolicy(ImportArchiveOnly, &archive) == nil {
		t.Fatal("档案不应允许建项")
	}
	live.FinanceMode = "NONE"
	if ValidateImportPolicy(ImportContinueLive, &live) == nil {
		t.Fatal("接续不应遗漏财务基线")
	}
	closed.Version = 2
	if ValidateImportPolicy(ImportHistoricalClosed, &closed) == nil {
		t.Fatal("未知版本不能放行")
	}
}
