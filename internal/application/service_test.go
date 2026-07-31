package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	temporalmocks "go.temporal.io/sdk/mocks"
)

type recordingRepository struct {
	ownerUserID  string
	contract     contract.Contract
	approvalMeta approval.Meta
	actions      []approval.Action
}

func (r *recordingRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return r.contract, nil
}

func (r *recordingRepository) ListContracts(_ context.Context, _, ownerUserID, _ string, _ int) ([]contract.Contract, error) {
	r.ownerUserID = ownerUserID
	return nil, nil
}

func (r *recordingRepository) CreateContract(context.Context, contract.Contract, string) error {
	return nil
}

func (r *recordingRepository) TransitionDirect(context.Context, string, string, uint64, contract.Status, string, string, string) error {
	return nil
}

func (r *recordingRepository) ListEnabledRules(context.Context, string) ([]approval.Rule, error) {
	return nil, nil
}

func (r *recordingRepository) ListRules(context.Context, string) ([]approval.Rule, error) {
	return nil, nil
}

func (r *recordingRepository) CreateRule(context.Context, approval.Rule, string) error {
	return nil
}

func (r *recordingRepository) UpdateRule(context.Context, approval.Rule, string) error {
	return nil
}

func (r *recordingRepository) DeleteRule(context.Context, string, string, uint64) error {
	return nil
}

func (r *recordingRepository) GetApprovalMeta(context.Context, string, string) (approval.Meta, error) {
	return r.approvalMeta, nil
}

func (r *recordingRepository) ListApprovalActions(context.Context, string, string) ([]approval.Action, error) {
	return r.actions, nil
}

func (r *recordingRepository) ListApprovals(context.Context, string, string, int) ([]approval.Summary, error) {
	return nil, nil
}

func (r *recordingRepository) ListTasks(context.Context, string, string, int) ([]approval.Task, error) {
	return nil, nil
}

func TestListContractsScopesNonManagerToAuthenticatedUser(t *testing.T) {
	repository := &recordingRepository{}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID:    "tenant-1",
		UserID:      "user-1",
		Permissions: map[string]bool{"contract.read": true},
	}

	if _, err := service.ListContracts(context.Background(), actor, "another-user", "", 50); err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.ownerUserID != actor.UserID {
		t.Fatalf("owner filter = %q, want authenticated user %q", repository.ownerUserID, actor.UserID)
	}
}

func TestListContractsIgnoresRequestedOwnerEvenWithLegacyManagerPermission(t *testing.T) {
	repository := &recordingRepository{}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID: "tenant-1",
		UserID:   "manager-1",
		Permissions: map[string]bool{
			"contract.read":   true,
			"contract.manage": true,
		},
	}

	if _, err := service.ListContracts(context.Background(), actor, "user-2", "", 50); err != nil {
		t.Fatalf("ListContracts() error = %v", err)
	}
	if repository.ownerUserID != actor.UserID {
		t.Fatalf("owner filter = %q, want authenticated user %q", repository.ownerUserID, actor.UserID)
	}
}

func TestDefaultApprovalNodesUseManifestRoleCodes(t *testing.T) {
	nodes := defaultNodes()
	if len(nodes) != 3 {
		t.Fatalf("defaultNodes() length = %d", len(nodes))
	}
	if nodes[0].RoleCode != "sales_director" || nodes[1].RoleCode != "tech_director" || nodes[2].RoleCode != "finance_director" {
		t.Fatalf("defaultNodes() role codes = %q, %q, %q", nodes[0].RoleCode, nodes[1].RoleCode, nodes[2].RoleCode)
	}
}

func TestGetContractRejectsNonOwnerEvenWithLegacyManagerPermission(t *testing.T) {
	repository := &recordingRepository{contract: contract.Contract{OwnerUserID: "user-2"}}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID: "tenant-1",
		UserID:   "manager-1",
		Permissions: map[string]bool{
			"contract.read":   true,
			"contract.manage": true,
		},
	}

	if _, err := service.GetContract(context.Background(), actor, "contract-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetContract() error = %v, want ErrForbidden", err)
	}
}

func TestSubmitContractRejectsNonOwnerEvenWithLegacyManagerPermission(t *testing.T) {
	repository := &recordingRepository{contract: contract.Contract{OwnerUserID: "user-2", Status: contract.StatusDraft}}
	service := &Service{Repo: repository}
	actor := Principal{
		TenantID: "tenant-1",
		UserID:   "manager-1",
		Permissions: map[string]bool{
			"contract.create": true,
			"contract.manage": true,
		},
	}

	if _, err := service.SubmitContract(context.Background(), actor, "contract-1", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("SubmitContract() error = %v, want ErrForbidden", err)
	}
}

func TestGetApprovalDetailReturnsContractToAssignedApprover(t *testing.T) {
	state := workflows.ApprovalState{ApprovalID: "approval-1", ContractID: "contract-1", Status: approval.StatusRunning}
	encoded := temporalmocks.NewEncodedValue(t)
	encoded.On("Get", mock.Anything).Run(func(arguments mock.Arguments) {
		target := arguments.Get(0).(*workflows.ApprovalState)
		*target = state
	}).Return(nil)
	temporal := temporalmocks.NewClient(t)
	temporal.On("QueryWorkflow", mock.Anything, "workflow-1", "run-1", workflows.StateQueryName).
		Return(encoded, nil)
	repository := &recordingRepository{
		contract: contract.Contract{ID: "contract-1", OwnerUserID: "applicant-1", Content: "contract body"},
		approvalMeta: approval.Meta{
			ID: "approval-1", TenantID: "tenant-1", ContractID: "contract-1",
			ApplicantUserID: "applicant-1", WorkflowID: "workflow-1", RunID: "run-1",
		},
		actions: []approval.Action{{ID: "action-1", Action: "comment", Comment: "reviewed"}},
	}
	service := &Service{Repo: repository, Temporal: temporal}
	actor := Principal{
		TenantID: "tenant-1", UserID: "approver-1",
		Permissions: map[string]bool{"approval.process": true},
	}

	detail, err := service.GetApprovalDetail(context.Background(), actor, "approval-1")
	if err != nil {
		t.Fatalf("GetApprovalDetail() error = %v", err)
	}
	if detail.Contract.Content != "contract body" || detail.State.ApprovalID != "approval-1" || len(detail.Actions) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestGetApprovalDetailRejectsUnrelatedUser(t *testing.T) {
	repository := &recordingRepository{approvalMeta: approval.Meta{ApplicantUserID: "applicant-1"}}
	service := &Service{Repo: repository}
	actor := Principal{TenantID: "tenant-1", UserID: "unrelated-user", Permissions: map[string]bool{}}

	if _, err := service.GetApprovalDetail(context.Background(), actor, "approval-1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("GetApprovalDetail() error = %v, want ErrForbidden", err)
	}
}
