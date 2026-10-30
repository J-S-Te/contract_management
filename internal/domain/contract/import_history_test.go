package contract

import "testing"

// TestHistoricalPolicyFailsClosed 验证首版不得重述经营或冒认签署核验。
// 参数：t为测试上下文。
// 返回值：无；不支持策略被接受时失败。
func TestHistoricalPolicyFailsClosed(t *testing.T) {
	for _, change := range []string{"report", "signing"} {
		p := &ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "EVIDENCE_ONLY", ReportMode: "HISTORICAL_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}
		if change == "report" {
			p.ReportMode = "INCLUDE_RESTATED"
		} else {
			p.SigningMode = "VERIFY_STAMPED_VERSION"
		}
		if ValidateImportPolicy(ImportHistoricalClosed, p) == nil {
			t.Fatalf("unsupported %s accepted", change)
		}
	}
}

// TestImportManifestMixedLifecycleRejected 验证混合生命周期在任何建单/确认前明确拒绝。
// 参数：t为领域测试上下文。
// 返回值：无；混合批次不能形成无法提交的PREVIEWED结果。
func TestImportManifestMixedLifecycleRejected(t *testing.T) {
	manifest := ImportManifest{SchemaVersion: 1, SourceSystem: "legacy", Items: []ImportItem{{Key: "normal", ExternalRecordID: "one", SourceFileRef: "one", Mode: ImportNormal}, {Key: "history", ExternalRecordID: "two", SourceFileRef: "two", Mode: ImportArchiveOnly}}}
	if e := manifest.Validate(100); e != ErrImportMixedLifecycle {
		t.Fatal("mixed lifecycle did not return explicit error", e)
	}
}
