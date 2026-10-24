package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
)

// 审批快照必须在 StartApproval 事务内落库，合同后续修改不得改变历史展示内容。
func TestApprovalSnapshotIsImmutableAfterContractEdit(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repository, tenant, contract.StatusDraft)
	ctx := context.Background()
	if err := repository.db.WithContext(ctx).Create(&paymentTermRecord{
		ID: ulid.Make().String(), TenantID: tenant, ContractID: item.ID, InstallmentNo: 1,
		DueDate: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), AmountMinor: 100000,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	approvalID := ulid.Make().String()
	in := workflows.StartApprovalActivityInput{
		ApprovalID: approvalID, TenantID: tenant, ContractID: item.ID, ExpectedVersion: 1,
		Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending,
		ContentHash: item.ContentHash, ApplicantUserID: item.OwnerUserID, WorkflowID: "wf-1", RunID: "run-1",
		RuleID: "rule-1", RuleVersion: 7,
		Nodes: []approval.Node{{ID: "node-1", Name: "销售审批", RoleCode: "sales_director", AssigneeIDs: []string{"approver-1"}, Countersign: approval.CountersignAny}},
	}
	if err := repository.StartApproval(ctx, in); err != nil {
		t.Fatalf("StartApproval: %v", err)
	}
	snapshot, found, err := repository.GetApprovalSnapshot(ctx, tenant, approvalID)
	if err != nil || !found {
		t.Fatalf("snapshot found=%v err=%v", found, err)
	}
	if snapshot.AmountMinor != 100000 || snapshot.Content != "contract body" || snapshot.Version != 1 || len(snapshot.PaymentTerms) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	// 审批中/拒绝后修改草稿：当前合同变化，但历史快照保持。
	if err := repository.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ? AND id = ?", tenant, item.ID).
		Updates(map[string]any{"amount_minor": 999900, "content": "new draft body"}).Error; err != nil {
		t.Fatal(err)
	}
	again, found, err := repository.GetApprovalSnapshot(ctx, tenant, approvalID)
	if err != nil || !found {
		t.Fatalf("snapshot reread found=%v err=%v", found, err)
	}
	if again.AmountMinor != 100000 || again.Content != "contract body" || again.PaymentTerms[0].Amount != "1000.00" {
		t.Fatalf("historical snapshot changed: %+v", again)
	}
}
