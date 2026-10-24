package mysql

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
)

// TestCompleteApprovalAppliesEffectsOnceOnMySQL 覆盖恢复竞争：
// 两个活动重试（包括并发重试）不得生成
// 第二组生命周期、Outbox、通知及副作用。
func TestCompleteApprovalAppliesEffectsOnceOnMySQL(t *testing.T) {
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
	if err := repository.StartApproval(ctx, workflows.StartApprovalActivityInput{
		ApprovalID: approvalID, TenantID: tenant, ContractID: item.ID, ExpectedVersion: 1,
		Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending,
		ContentHash: item.ContentHash, ApplicantUserID: item.OwnerUserID, WorkflowID: "wf-idem", RunID: "run-idem",
		Nodes: []approval.Node{{ID: "node-1", Name: "销售审批", RoleCode: "sales_director", AssigneeIDs: []string{"approver-1"}, Countersign: approval.CountersignAny}},
	}); err != nil {
		t.Fatalf("StartApproval: %v", err)
	}
	var beforeLifecycles int64
	if err := repository.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM con_contract_lifecycle_event WHERE tenant_id=? AND contract_id=?`, tenant, item.ID).Scan(&beforeLifecycles).Error; err != nil {
		t.Fatal(err)
	}
	input := workflows.CompleteApprovalActivityInput{ApprovalID: approvalID, TenantID: tenant, ContractID: item.ID, ActorUserID: "approver-1", Status: approval.StatusApproved, WorkflowID: "wf-idem"}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errorsCh <- repository.CompleteApproval(ctx, input) }()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent CompleteApproval: %v", err)
		}
	}
	var lifecycles, projectOutbox, financialOutbox, notifications int64
	for query, target := range map[string]*int64{
		`SELECT COUNT(*) FROM con_contract_lifecycle_event WHERE tenant_id=? AND contract_id=?`:   &lifecycles,
		`SELECT COUNT(*) FROM con_project_delivery_outbox WHERE tenant_id=? AND contract_id=?`:    &projectOutbox,
		`SELECT COUNT(*) FROM con_financial_effective_outbox WHERE tenant_id=? AND contract_id=?`: &financialOutbox,
		`SELECT COUNT(*) FROM con_notification_outbox WHERE tenant_id=? AND contract_id=?`:        &notifications,
	} {
		if err := repository.db.WithContext(ctx).Raw(query, tenant, item.ID).Scan(target).Error; err != nil {
			t.Fatal(err)
		}
	}
	if lifecycles-beforeLifecycles != 2 || projectOutbox != 1 || notifications != 0 {
		t.Fatalf("effects delta=%d project=%d financial=%d notifications=%d, want lifecycle delta 2/project 1/notifications 0", lifecycles-beforeLifecycles, projectOutbox, financialOutbox, notifications)
	}
	// 提交后的第三次重试为空操作。
	if err := repository.CompleteApproval(ctx, input); err != nil {
		t.Fatalf("retry CompleteApproval: %v", err)
	}
	var after int64
	if err := repository.db.WithContext(ctx).Raw(`SELECT COUNT(*) FROM con_contract_lifecycle_event WHERE tenant_id=? AND contract_id=?`, tenant, item.ID).Scan(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after != lifecycles {
		t.Fatalf("retry changed lifecycles %d -> %d", lifecycles, after)
	}
}
