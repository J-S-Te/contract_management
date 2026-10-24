package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	temporalmocks "go.temporal.io/sdk/mocks"
)

func encodedApprovalState(t *testing.T, state workflows.ApprovalState) *temporalmocks.Value {
	t.Helper()
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	return encoded
}

// Signal 被接收后必须等查询确认节点实际转派才返回，并携带节点 ID 与稳定命令 ID。
func TestReassignApprovalNodeWaitsForWorkflowExecution(t *testing.T) {
	before := workflows.ApprovalState{
		ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning, CurrentNodeIndex: 0,
		Nodes: []workflows.RuntimeNode{{Node: approval.Node{ID: "node-2", AssigneeIDs: []string{"user-leaver"}}, Status: approval.NodeActive}},
	}
	after := before
	after.Nodes = []workflows.RuntimeNode{{Node: approval.Node{ID: "node-2", AssigneeIDs: []string{"user-successor"}}, Status: approval.NodeActive}}
	temporal := temporalmocks.NewClient(t)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).Return(encodedApprovalState(t, before), nil).Once()
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).Return(encodedApprovalState(t, after), nil)
	temporal.On("SignalWorkflow", mock.Anything, "workflow-1", "run-1", workflows.CommandSignalName, mock.MatchedBy(func(command workflows.ApprovalCommand) bool {
		return command.Action == workflows.ActionTransfer && command.TargetNodeID == "node-2" &&
			command.CommandID == handoverCommandID("handover:batch-1:item-1") &&
			len(command.TargetUserIDs) == 1 && command.TargetUserIDs[0] == "user-successor"
	})).Return(nil)
	service := &Service{
		Repo: &recordingRepository{approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", Status: approval.StatusRunning,
			WorkflowID: "workflow-1", RunID: "run-1",
		}},
		Temporal: temporal,
	}

	evidence, err := service.ReassignApprovalNode(context.Background(), "tenant-1", "approval-1", "node-2", "user-leaver", "user-successor", "handover", "handover:batch-1:item-1")
	if err != nil {
		t.Fatalf("ReassignApprovalNode() error = %v", err)
	}
	if evidence == "" {
		t.Fatal("missing evidence reference")
	}
}

// 已实际转派但回执丢失时，同一幂等键重试应直接返回成功且不再发送 Signal。
func TestReassignApprovalNodeIsIdempotentAfterExecution(t *testing.T) {
	applied := workflows.ApprovalState{
		ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning, CurrentNodeIndex: 0,
		Nodes: []workflows.RuntimeNode{{Node: approval.Node{ID: "node-2", AssigneeIDs: []string{"user-successor"}}, Status: approval.NodeActive}},
	}
	temporal := temporalmocks.NewClient(t)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).Return(encodedApprovalState(t, applied), nil)
	service := &Service{
		Repo: &recordingRepository{approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", Status: approval.StatusRunning,
			WorkflowID: "workflow-1", RunID: "run-1",
		}},
		Temporal: temporal,
	}

	if _, err := service.ReassignApprovalNode(context.Background(), "tenant-1", "approval-1", "node-2", "user-leaver", "user-successor", "handover", "handover:batch-1:item-1"); err != nil {
		t.Fatalf("idempotent ReassignApprovalNode() error = %v", err)
	}
	temporal.AssertNotCalled(t, "SignalWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// 创建合同时冻结检查必须针对真实负责人（当前操作者），而不是尚未赋值的空 OwnerUserID。
func TestCreateContractChecksFrozenActorNotUnassignedOwner(t *testing.T) {
	templates := &memoryTemplateRepository{items: map[string]contracttemplate.Template{}}
	template := contracttemplate.Template{ID: "template-1", TenantID: "tenant-1", Name: "标准模板", Content: applicationTestDOCX(t, "合同正文")}
	templates.items[template.ID] = template
	checker := &handoverFreezeCheckerStub{found: true, freeze: HandoverFreeze{ID: "freeze-1", BatchID: "batch-9", UserID: "user-leaver", Status: HandoverFreezeFrozen}}
	service := &Service{Templates: templates, HandoverFreeze: checker}
	actor := Principal{
		TenantID: "tenant-1", UserID: "user-leaver", IdentityID: "user-leaver",
		Permissions:      map[string]bool{"contract.create": true},
		PermissionScopes: allowAllScope("contract.create"),
	}
	input := contract.Contract{
		Title: "被冻结用户的合同", Type: "直签", TemplateID: template.ID,
		AmountMinor: 100000, Currency: "CNY",
		ServiceItems: []contract.ServiceItem{{ServiceType: "等保测评", Systems: []contract.SystemInfo{{Name: "核心系统", Level: "一级"}}}},
	}

	if _, err := service.CreateContract(context.Background(), actor, input); !errors.Is(err, ErrHandoverFrozen) {
		t.Fatalf("CreateContract() error = %v, want ErrHandoverFrozen", err)
	}
	if checker.userID != actor.UserID {
		t.Fatalf("freeze lookup used %q, want actual owner %q", checker.userID, actor.UserID)
	}
}
