package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestImportManifestFinanceBoundary 验证新历史项不接续财务，旧项只保留原策略恢复。
// 参数：t为清单写边界测试上下文。
// 返回值：无；策略绕过、旧项被误拒绝或失败写入改变修订时失败。
func TestImportManifestFinanceBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, finance, previous string
		facts, reject           bool
	}{
		{"新结清无财务", "NONE", "", false, false},
		{"新项不得以凭证模式迁移", "EVIDENCE_ONLY", "", false, true},
		{"新项不得重建财务", "FULL_REPLAY", "", false, true},
		{"原凭证项保留", "EVIDENCE_ONLY", "EVIDENCE_ONLY", false, false},
		{"原接续策略保留", "FULL_REPLAY", "FULL_REPLAY", false, false},
		{"旧项可显式退出财务", "NONE", "EVIDENCE_ONLY", false, false},
		{"旧凭证项不得升级财务", "FULL_REPLAY", "EVIDENCE_ONLY", false, true},
		{"无财务不得夹带财务事实", "NONE", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := contract.ImportItem{Key: "one", ExternalRecordID: "old-1", SourceFileRef: "source",
				Mode: contract.ImportHistoricalClosed, Policy: &contract.ImportPolicy{Version: 1,
					ProjectMode: "NONE", FinanceMode: tc.finance, ReportMode: "HISTORICAL_SEPARATE",
					ResponsibilityMode: "INACTIVE", SigningMode: "DECLARED_ONLY"}}
			if tc.facts {
				facts := []contract.ImportFact{{SourceSystem: "old", FactType: "RECEIPT", ExternalFactID: "r1"}}
				item.HistoryEvidence = &contract.ImportHistoryEvidence{ExpectedFacts: &facts}
			}
			repo := &importPreviewRepository{batch: contract.ImportBatch{ID: "batch", TenantID: "tenant",
				ActorID: "actor", SourceSystem: "legacy", Version: 1, Revision: 1, State: "DRAFT",
				ExpiresAt: time.Now().Add(time.Hour), Sources: []contract.ImportSource{
					{ID: "source", ItemKey: "one", State: "READY"}}}}
			if tc.previous != "" {
				old := item
				policy := *item.Policy
				policy.FinanceMode = tc.previous
				old.Policy = &policy
				repo.batch.Items = []contract.ImportItemState{{ID: "old-item", Item: old}}
			}
			actor := Principal{TenantID: "tenant", UserID: "actor",
				Permissions:      map[string]bool{"contract.import.preview": true},
				PermissionScopes: allowAllScope("contract.import.preview")}
			svc := &Service{Repo: repo, ContractImportEnabled: true}
			err := svc.SetImportManifest(context.Background(), actor, "batch", 1,
				contract.ImportManifest{SchemaVersion: 1, SourceSystem: "legacy", Items: []contract.ImportItem{item}})
			if tc.reject && !errors.Is(err, ErrValidation) || !tc.reject && err != nil {
				t.Fatalf("reject=%v, error=%v", tc.reject, err)
			}
			if tc.reject && repo.batch.Revision != 1 {
				t.Fatal("被拒绝的财务策略不得改变修订")
			}
		})
	}
}
