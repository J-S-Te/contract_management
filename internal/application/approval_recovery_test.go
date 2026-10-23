package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	temporalmocks "go.temporal.io/sdk/mocks"
)

type approvalRecoveryRepository struct {
	*recordingRepository
	runtime   workflows.ApprovalState
	found     bool
	completed *workflows.CompleteApprovalActivityInput
}

func (r *approvalRecoveryRepository) GetApprovalRuntimeState(context.Context, string, string) (workflows.ApprovalState, bool, error) {
	return r.runtime, r.found, nil
}

func (r *approvalRecoveryRepository) CompleteApproval(_ context.Context, in workflows.CompleteApprovalActivityInput) error {
	r.completed = &in
	return nil
}

func recoveryService(t *testing.T, status enums.WorkflowExecutionStatus, runtime workflows.ApprovalState) (*Service, *approvalRecoveryRepository) {
	t.Helper()
	repository := &approvalRecoveryRepository{
		recordingRepository: &recordingRepository{approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", Kind: approval.KindContract,
			Status: approval.StatusRunning, WorkflowID: "workflow-1", RunID: "run-1",
		}},
		runtime: runtime, found: true,
	}
	temporal := temporalmocks.NewClient(t)
	temporal.On("DescribeWorkflowExecution", mock.Anything, "workflow-1", "run-1").Return(&workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}, nil)
	return &Service{Repo: repository, Temporal: temporal}, repository
}

func recoveryActor() Principal {
	return Principal{TenantID: "tenant-1", UserID: "admin-1", Permissions: map[string]bool{"approval.manage": true}}
}

// 工作流已失败但最后一个节点已通过时，管理端可按持久化状态幂等收口为生效。
func TestRecoverApprovalCompletesStoppedWorkflow(t *testing.T) {
	runtime := workflows.ApprovalState{
		Status: approval.StatusRunning, CurrentNodeIndex: 1,
		Nodes: []workflows.RuntimeNode{{Node: approval.Node{ID: "node-1"}, Status: approval.NodeApproved}},
	}
	service, repository := recoveryService(t, enums.WORKFLOW_EXECUTION_STATUS_FAILED, runtime)
	if err := service.RecoverApproval(context.Background(), recoveryActor(), "approval-1"); err != nil {
		t.Fatalf("RecoverApproval() error = %v", err)
	}
	if repository.completed == nil {
		t.Fatal("CompleteApproval was not called")
	}
	if repository.completed.Status != approval.StatusApproved || repository.completed.TargetStatus != contract.StatusActive ||
		repository.completed.ActorUserID != "admin-1" {
		t.Fatalf("completed input = %+v", repository.completed)
	}
}

// 工作流仍在运行或停在未完成节点时不得人工收口。
func TestRecoverApprovalRejectsRunningWorkflowAndMidNodeState(t *testing.T) {
	runtime := workflows.ApprovalState{
		Status: approval.StatusRunning, CurrentNodeIndex: 0,
		Nodes: []workflows.RuntimeNode{{Node: approval.Node{ID: "node-1"}, Status: approval.NodeActive}},
	}
	service, repository := recoveryService(t, enums.WORKFLOW_EXECUTION_STATUS_RUNNING, runtime)
	if err := service.RecoverApproval(context.Background(), recoveryActor(), "approval-1"); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("running workflow error = %v, want ErrStateConflict", err)
	}
	if repository.completed != nil {
		t.Fatal("running workflow must not be completed")
	}

	failedService, failedRepository := recoveryService(t, enums.WORKFLOW_EXECUTION_STATUS_FAILED, runtime)
	if err := failedService.RecoverApproval(context.Background(), recoveryActor(), "approval-1"); !errors.Is(err, apperrors.ErrStateConflict) {
		t.Fatalf("mid-node error = %v, want ErrStateConflict", err)
	}
	if failedRepository.completed != nil {
		t.Fatal("mid-node state must not be auto-approved")
	}
}

func TestRecoverApprovalRequiresManagePermission(t *testing.T) {
	repository := &approvalRecoveryRepository{recordingRepository: &recordingRepository{approvalMeta: approval.Meta{
		ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1", Status: approval.StatusRunning,
		WorkflowID: "workflow-1", RunID: "run-1",
	}}}
	service := &Service{Repo: repository, Temporal: temporalmocks.NewClient(t)}
	actor := Principal{TenantID: "tenant-1", UserID: "sales-1"}
	if err := service.RecoverApproval(context.Background(), actor, "approval-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("permission error = %v, want ErrForbidden", err)
	}
	if repository.completed != nil {
		t.Fatal("forbidden recovery must not complete")
	}
}
