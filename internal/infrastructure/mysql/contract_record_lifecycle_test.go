package mysql

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

// TestRecordSafetyBoundaries 用途：验证安全候选、业务阻断与恢复不派生边界。
// 参数：t 为测试上下文，每个子场景只改变一类事实。
// 返回值：无；候选资格与公开原因不符时失败。
func TestRecordSafetyBoundaries(t *testing.T) {
	now := time.Now().UTC()
	policy, _ := json.Marshal(contract.ImportPolicy{Version: 1, ProjectMode: "NONE", FinanceMode: "NONE",
		ReportMode: "ARCHIVE_SEPARATE", ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"})
	cases := []struct {
		name  string
		row   contractRecord
		facts recordEvidence
		code  string
	}{
		{"never_approved_draft", contractRecord{Status: "draft"}, recordEvidence{ImportSettled: true}, ""},
		{"restore_draft", contractRecord{Status: "draft", DeletedAt: gorm.DeletedAt{Time: now, Valid: true}}, recordEvidence{ImportSettled: true}, ""},
		{"settled_archive", contractRecord{OriginKind: "IMPORTED", ImportMode: "ARCHIVE_ONLY", ImportPolicyJSON: policy, ImportConfirmedAt: &now, Status: "archived"}, recordEvidence{ImportSettled: true}, ""},
		{"prior_approved", contractRecord{Status: "draft"}, recordEvidence{Approved: 1, ImportSettled: true}, "DOWNSTREAM_UNKNOWN"},
		{"active_without_outbox", contractRecord{Status: "active"}, recordEvidence{ImportSettled: true}, "DOWNSTREAM_UNKNOWN"},
		{"not_applied_approval", contractRecord{Status: "draft"}, recordEvidence{InFlight: 1, ImportSettled: true}, "APPROVAL_IN_FLIGHT"},
		{"pending_project", contractRecord{Status: "draft"}, recordEvidence{Project: 1, ImportSettled: true}, "PROJECT_ASSOCIATED"},
		{"dead_finance", contractRecord{Status: "draft"}, recordEvidence{Finance: 1, ImportSettled: true}, "FINANCE_ASSOCIATED"},
		{"unsettled_import", contractRecord{Status: "draft"}, recordEvidence{}, "IMPORT_NOT_SETTLED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := recordSafetyBlockers(tc.row, tc.facts)
			if tc.code == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected blockers: %+v", got)
				}
				return
			}
			for _, blocker := range got {
				if blocker.Code == tc.code {
					return
				}
			}
			t.Fatalf("missing %s in %+v", tc.code, got)
		})
	}
}

// TestRecordMetadataKeepsFrozenJSON 用途：确保内部墓碑不会改变旧导入摘要字节。
// 参数：t 为测试上下文，比较同一合同有无管理元数据的 JSON。
// 返回值：无；旧序列化形状发生漂移时失败。
func TestRecordMetadataKeepsFrozenJSON(t *testing.T) {
	row := contract.Contract{ID: "C", Status: contract.StatusDraft, Version: 1}
	before, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	row.DeletedAt, row.DeletedBy, row.DeleteReason = &now, "admin", "误建"
	after, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("internal metadata changed frozen JSON")
	}
}
