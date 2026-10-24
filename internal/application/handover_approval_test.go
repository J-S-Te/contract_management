package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

type approvalDispatcherStub struct {
	evidence   string
	err        error
	calls      int
	approvalID string
	nodeID     string
	from, to   string
}

func (stub *approvalDispatcherStub) DispatchApprovalTransfer(_ context.Context, _, approvalID, nodeID, from, to, _, _ string) (string, error) {
	stub.calls++
	stub.approvalID, stub.nodeID, stub.from, stub.to = approvalID, nodeID, from, to
	return stub.evidence, stub.err
}

func approvalTransferInput() HandoverTransferInput {
	return HandoverTransferInput{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", RequestID: "request-1",
		ResourceType: HandoverResourceApprovalTask, ResourceID: "approval-1:node-2",
		FromUserID: "user-leaver", ToUserID: "user-successor",
		ExpectedVersion: 1, IdempotencyKey: "handover:batch-1:item-1",
	}
}

// 审批节点转派必须走权威工作流命令，且不得落到"直接改负责人"的仓储路径。
func TestHandoverApprovalTransferDispatchesWorkflowCommand(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	dispatcher := &approvalDispatcherStub{evidence: "workflow:approval-1/run-1"}
	service := handoverServiceFor(t, repository)
	service.SetApprovalTransferDispatcher(dispatcher)

	receipt, err := service.Transfer(context.Background(), approvalTransferInput())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != HandoverReceiptApplied || receipt.EvidenceRef != "workflow:approval-1/run-1" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if dispatcher.calls != 1 || dispatcher.approvalID != "approval-1" || dispatcher.nodeID != "node-2" ||
		dispatcher.from != "user-leaver" || dispatcher.to != "user-successor" {
		t.Fatalf("dispatcher call = %+v", dispatcher)
	}
	if repository.transferCalls != 0 {
		t.Fatal("approval transfer must not use the contract ownership repository path")
	}
	stored, ok := repository.receipts["tenant-1|handover:batch-1:item-1"]
	if !ok || stored.Status != HandoverReceiptApplied || stored.EvidenceRef != "workflow:approval-1/run-1" ||
		stored.ResourceID != "approval-1:node-2" || stored.ToUserID != "user-successor" {
		t.Fatalf("approval receipt was not persisted: %+v", stored)
	}
}

// 未接线派发通道时按 RETRYABLE 返回，平台会退避重试；不得静默当作成功。
func TestHandoverApprovalTransferWithoutDispatcherIsRetryable(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	service := handoverServiceFor(t, repository)

	receipt, err := service.Transfer(context.Background(), approvalTransferInput())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != HandoverReceiptRetryable || receipt.ReasonCode != "APPROVAL_DISPATCH_UNAVAILABLE" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if repository.transferCalls != 0 {
		t.Fatal("no repository write may happen without a dispatcher")
	}
}

// 工作流命令失败同样按 RETRYABLE 上报，保留真实原因。
func TestHandoverApprovalTransferCommandFailureIsRetryable(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	service := handoverServiceFor(t, repository)
	service.SetApprovalTransferDispatcher(&approvalDispatcherStub{err: errors.New("temporal unavailable")})

	receipt, err := service.Transfer(context.Background(), approvalTransferInput())
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != HandoverReceiptRetryable || receipt.ReasonCode != "APPROVAL_COMMAND_FAILED" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

// 审批责任标识不合法必须直接拒绝，避免把未知资源当作审批项处理。
func TestHandoverApprovalTransferRejectsMalformedResource(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	service := handoverServiceFor(t, repository)
	service.SetApprovalTransferDispatcher(&approvalDispatcherStub{})

	input := approvalTransferInput()
	input.ResourceID = "approval-without-node"
	if _, err := service.Transfer(context.Background(), input); !errors.Is(err, ErrHandoverValidation) {
		t.Fatalf("err = %v, want ErrHandoverValidation", err)
	}
}

// 合同负责人转派仍走原仓储路径（路由不得破坏既有语义）。
func TestHandoverContractTransferStillUsesRepository(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	service := handoverServiceFor(t, repository)
	service.SetApprovalTransferDispatcher(&approvalDispatcherStub{evidence: "workflow:unused"})

	receipt, err := service.Transfer(context.Background(), HandoverTransferInput{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", ResourceType: "contract", ResourceID: "c-1",
		FromUserID: "user-leaver", ToUserID: "user-successor", ExpectedVersion: 3, IdempotencyKey: "handover:batch-1:item-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != HandoverReceiptApplied || repository.transferCalls != 1 {
		t.Fatalf("receipt = %+v calls = %d", receipt, repository.transferCalls)
	}
}

var _ = time.Now
