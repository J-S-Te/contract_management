package application

import (
	"context"
	"errors"
	"testing"
)

type approvalWorkflowTransfersStub struct {
	evidence string
	err      error
	calls    int
	nodeID   string
}

func (stub *approvalWorkflowTransfersStub) ReassignApprovalNode(_ context.Context, _, _, nodeID, _, _, _, _ string) (string, error) {
	stub.calls++
	stub.nodeID = nodeID
	return stub.evidence, stub.err
}

// 适配器把交接派发端口原样接到权威工作流命令实现，并回传证据引用。
func TestHandoverApprovalDispatcherDelegatesToWorkflowTransfers(t *testing.T) {
	transfers := &approvalWorkflowTransfersStub{evidence: "approval-workflow:wf-1/run-1"}
	dispatcher := NewHandoverApprovalDispatcher(transfers)

	evidence, err := dispatcher.DispatchApprovalTransfer(context.Background(), "tenant-1", "approval-1", "node-2", "user-leaver", "user-successor", "handover:batch-1:item-1", "reason")
	if err != nil {
		t.Fatal(err)
	}
	if evidence != "approval-workflow:wf-1/run-1" || transfers.calls != 1 || transfers.nodeID != "node-2" {
		t.Fatalf("evidence=%q calls=%d node=%q", evidence, transfers.calls, transfers.nodeID)
	}
}

// 未配置实现时必须报错（上层会据此给出 RETRYABLE），不得静默成功。
func TestHandoverApprovalDispatcherWithoutTransfersFails(t *testing.T) {
	dispatcher := NewHandoverApprovalDispatcher(nil)
	if _, err := dispatcher.DispatchApprovalTransfer(context.Background(), "tenant-1", "approval-1", "node-2", "a", "b", "handover:batch-1:item-1", "reason"); err == nil {
		t.Fatal("missing workflow transfers must fail")
	}
}

// 工作流命令失败必须原样上抛，交由上层判定为可重试。
func TestHandoverApprovalDispatcherPropagatesFailure(t *testing.T) {
	transfers := &approvalWorkflowTransfersStub{err: errors.New("temporal unavailable")}
	dispatcher := NewHandoverApprovalDispatcher(transfers)
	if _, err := dispatcher.DispatchApprovalTransfer(context.Background(), "tenant-1", "approval-1", "node-2", "a", "b", "handover:batch-1:item-1", "reason"); err == nil {
		t.Fatal("workflow failure must propagate")
	}
}

// 责任清单标注只补空缺：仓储给出的审批类别不得被覆盖成合同负责人。
func TestHandoverResponsibilitiesPreserveRepositoryCategory(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	repository.responsibilities = append(repository.responsibilities, HandoverResponsibility{
		ResourceType: HandoverResourceApprovalTask, ResourceID: "approval-1:node-2", ResourceName: "合同审批节点",
		OwnerUserID: "user-leaver", Category: HandoverCategoryApprovalAssignee, Version: 1,
	})
	service := handoverServiceFor(t, repository)

	items, _, err := service.Responsibilities(context.Background(), "tenant-1", "user-leaver", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]HandoverResponsibility{}
	for _, item := range items {
		byID[item.ResourceID] = item
	}
	approval := byID["approval-1:node-2"]
	if approval.Category != HandoverCategoryApprovalAssignee || approval.ResourceType != HandoverResourceApprovalTask {
		t.Fatalf("approval responsibility was mislabelled: %+v", approval)
	}
	contract := byID["c-1"]
	if contract.Category != HandoverCategoryContractOwner || contract.ResourceType != "contract" {
		t.Fatalf("contract responsibility missing default category: %+v", contract)
	}
}
