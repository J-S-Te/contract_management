package mysql

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
)

// contractApprovalStart 构造 draft→pending 的合同审批启动输入。
// 参数：item 为待提交合同，approvalID/workflowID 为审批与工作流标识。
// 返回值：可直接交给 StartApproval 的输入。
func contractApprovalStart(item contract.Contract, approvalID, workflowID string) workflows.StartApprovalActivityInput {
	return workflows.StartApprovalActivityInput{
		ApprovalID: approvalID, TenantID: item.TenantID, ContractID: item.ID, ExpectedVersion: item.Version,
		Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending,
		ContentHash: item.ContentHash, ApplicantUserID: item.OwnerUserID, WorkflowID: workflowID, RunID: ulid.Make().String(),
		Nodes: []approval.Node{{ID: "node-1", Name: "销售审批", RoleCode: "sales_director", AssigneeIDs: []string{"approver-1"}, Countersign: approval.CountersignAny}},
	}
}

// runningContractApprovals 统计某合同的在途合同审批数，用于并发回归断言。
// 参数：t 为测试上下文，repository 为仓储，tenant/contractID 为身份。
// 返回值：在途实例数；查询失败直接失败用例。
func runningContractApprovals(t *testing.T, repository *Repository, tenant, contractID string) int64 {
	t.Helper()
	var running int64
	if err := repository.db.Model(&approvalInstanceRecord{}).
		Where("tenant_id = ? AND contract_id = ? AND kind = ? AND status = ?", tenant, contractID, string(approval.KindContract), string(approval.StatusRunning)).
		Count(&running).Error; err != nil {
		t.Fatalf("count running approvals: %v", err)
	}
	return running
}

// TestTransitionDirectRejectsPendingContractWithRunningApprovalE2E 覆盖 M-6：合同审批在途时
// pending→draft 的直迁必须 409 且不改状态/版本，而合法的审批撤回链路（CompleteApproval withdrawn
// → draft）不得被堵死。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestTransitionDirectRejectsPendingContractWithRunningApprovalE2E(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	ctx := context.Background()
	item := stampedE2EContract(t, repository, tenant, contract.StatusDraft)
	start := contractApprovalStart(item, ulid.Make().String(), "wf-m6-direct")
	if err := repository.StartApproval(ctx, start); err != nil {
		t.Fatalf("StartApproval: %v", err)
	}
	pending, err := repository.GetContract(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != contract.StatusPending || runningContractApprovals(t, repository, tenant, item.ID) != 1 {
		t.Fatalf("precondition status=%s running=%d", pending.Status, runningContractApprovals(t, repository, tenant, item.ID))
	}

	err = repository.TransitionDirect(ctx, tenant, item.ID, pending.Version, contract.StatusDraft, item.OwnerUserID, "审批在途时直接撤回", ulid.Make().String())
	if !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("in-flight approval direct transition err=%v, want ErrStateConflict", err)
	}
	after, err := repository.GetContract(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != contract.StatusPending || after.Version != pending.Version {
		t.Fatalf("rejected transition mutated contract: status=%s version=%d, want %s/%d", after.Status, after.Version, pending.Status, pending.Version)
	}

	// 撤回是独立链路：CompleteApproval 落库 pending→draft 必须仍然成功。
	if err := repository.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{
		ApprovalID: start.ApprovalID, TenantID: tenant, ContractID: item.ID, ActorUserID: item.OwnerUserID,
		Status: approval.StatusWithdrawn, Reason: "申请人撤回", WorkflowID: "wf-m6-direct",
	}); err != nil {
		t.Fatalf("withdraw path blocked: %v", err)
	}
	withdrawn, err := repository.GetContract(ctx, tenant, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if withdrawn.Status != contract.StatusDraft || withdrawn.Version != pending.Version+1 {
		t.Fatalf("withdraw result status=%s version=%d, want draft/%d", withdrawn.Status, withdrawn.Version, pending.Version+1)
	}
}

// TestConcurrentContractApprovalStartAllowsExactlyOneE2E 覆盖 M-6 并发回归：同一 draft 合同并发
// 10 次 StartApproval，只能有一次成功，最终只有一份在途合同审批。
// 参数：t 为测试上下文。
// 返回值：无；实际行为偏离断言时用例失败。
func TestConcurrentContractApprovalStartAllowsExactlyOneE2E(t *testing.T) {
	repository, tenant := draftE2ERepository(t)
	for round := 0; round < 10; round++ {
		item := stampedE2EContract(t, repository, tenant, contract.StatusDraft)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		start := make(chan struct{})
		results := make(chan error, 10)
		var wg sync.WaitGroup
		for attempt := 0; attempt < 10; attempt++ {
			in := contractApprovalStart(item, ulid.Make().String(), fmt.Sprintf("wf-m6-race-%d-%d", round, attempt))
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- repository.StartApproval(ctx, in)
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		cancel()

		success, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				success++
			case errors.Is(err, apperrors.ErrStateConflict), errors.Is(err, apperrors.ErrVersionConflict):
				conflicts++
			default:
				t.Fatalf("round %d: unexpected concurrent StartApproval error: %v", round, err)
			}
		}
		if success != 1 || conflicts != 9 {
			t.Fatalf("round %d: success=%d conflicts=%d, want 1/9", round, success, conflicts)
		}
		if running := runningContractApprovals(t, repository, tenant, item.ID); running != 1 {
			t.Fatalf("round %d: running contract approvals=%d, want 1", round, running)
		}
		loaded, err := repository.GetContract(context.Background(), tenant, item.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Status != contract.StatusPending {
			t.Fatalf("round %d: contract status=%s, want pending", round, loaded.Status)
		}
	}
}
