package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"
)

type statusAuthorizationRepository struct {
	*recordingRepository
	transitions int
}

func (r *statusAuthorizationRepository) TransitionDirect(_ context.Context, tenantID, contractID string, version uint64, target contract.Status, actorUserID, reason, key string) error {
	if tenantID != r.contract.TenantID || contractID != r.contract.ID || version != r.contract.Version || actorUserID != "sales-1" || reason != "业务已完成" || key == "" {
		return errors.New("invalid transition persistence arguments")
	}
	r.transitions++
	r.contract.Status = target
	r.contract.Version++
	return nil
}

func statusAuthorizationActor(roles ...string) Principal {
	return Principal{
		TenantID: "tenant-1", UserID: "sales-1", IdentityID: "sales-1", Roles: roles,
		Permissions: map[string]bool{"contract.read": true, "contract.edit": true},
		PermissionScopes: map[string]contract.ScopeFilter{
			"contract.read": {AllowAll: true, OrganizationIDs: []string{"org-1"}, ProjectIDs: []string{"project-1"}},
			"contract.edit": {AllowAll: true, OrganizationIDs: []string{"org-1"}, ProjectIDs: []string{"project-1"}},
		},
	}
}

func statusAuthorizationContract(tenantID, owner string, from contract.Status) contract.Contract {
	return contract.Contract{
		ID: "contract-1", TenantID: tenantID, OwnerUserID: owner, OwnerIdentityID: owner,
		OwnerOrgID: "org-1", ProjectID: "project-1", CreatedBy: owner, Status: from, Version: 3,
	}
}

// 租户或应用全范围的平台声明不得允许普通销售通过
// 任一直达完成路径完成其他负责人的合同。
func TestChangeStatusEnforcesSalesOwnershipForDirectCompletion(t *testing.T) {
	for _, from := range []contract.Status{contract.StatusInProgress, contract.StatusPendingPay} {
		for _, tc := range []struct {
			name, tenant, owner string
			roles               []string
			allowed             bool
		}{
			{"sales_own", "tenant-1", "sales-1", []string{"sales"}, true},
			{"sales_other", "tenant-1", "sales-2", []string{"sales"}, false},
			{"sales_cross_tenant", "tenant-2", "sales-1", []string{"sales"}, false},
			{"admin_other", "tenant-1", "sales-2", []string{"admin"}, true},
			{"admin_sales_other", "tenant-1", "sales-2", []string{"admin", "sales"}, true},
			{"admin_cross_tenant", "tenant-2", "sales-2", []string{"admin"}, false},
		} {
			t.Run(string(from)+"/"+tc.name, func(t *testing.T) {
				repo := &statusAuthorizationRepository{recordingRepository: &recordingRepository{contract: statusAuthorizationContract(tc.tenant, tc.owner, from)}}
				svc := &Service{Repo: repo}
				_, err := svc.ChangeStatus(context.Background(), statusAuthorizationActor(tc.roles...), "contract-1", 3, contract.StatusCompleted, "业务已完成")
				if !tc.allowed {
					if !errors.Is(err, ErrForbidden) || repo.transitions != 0 || repo.contract.Status != from || repo.contract.Version != 3 {
						t.Fatalf("unauthorized completion: err=%v writes=%d status=%s version=%d", err, repo.transitions, repo.contract.Status, repo.contract.Version)
					}
					return
				}
				if err != nil || repo.transitions != 1 || repo.contract.Status != contract.StatusCompleted || repo.contract.Version != 4 {
					t.Fatalf("authorized completion: err=%v writes=%d status=%s version=%d", err, repo.transitions, repo.contract.Status, repo.contract.Version)
				}
			})
		}
	}
}

type statusAuthorizationPersonnel struct {
	calls int
}

func (p *statusAuthorizationPersonnel) ListEligibleUsers(context.Context, Principal, []string) ([]UserReference, error) {
	p.calls++
	return []UserReference{{UserID: "admin-1", Roles: []string{"admin"}}}, nil
}

// 解析审批人或启动 Temporal 前必须校验归属；
// 已授权的转换仍须走审批，不能直接写入。
func TestChangeStatusEnforcesSalesOwnershipBeforeApproval(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, owner string
		roles               []string
		allowed             bool
	}{
		{"sales_own", "tenant-1", "sales-1", []string{"sales"}, true},
		{"sales_other", "tenant-1", "sales-2", []string{"sales"}, false},
		{"sales_cross_tenant", "tenant-2", "sales-1", []string{"sales"}, false},
		{"admin_other", "tenant-1", "sales-2", []string{"admin"}, true},
		{"admin_cross_tenant", "tenant-2", "sales-2", []string{"admin"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &statusAuthorizationRepository{recordingRepository: &recordingRepository{contract: statusAuthorizationContract(tc.tenant, tc.owner, contract.StatusInProgress)}}
			personnel := &statusAuthorizationPersonnel{}
			temporal := temporalmocks.NewClient(t)
			run := temporalmocks.NewWorkflowRun(t)
			run.On("GetID").Return("workflow-1").Maybe()
			run.On("GetRunID").Return("run-1").Maybe()
			temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.StatusChangeWorkflowName, mock.Anything).Return(run, nil).Maybe()
			svc := &Service{Repo: repo, Personnel: personnel, Temporal: temporal}
			result, err := svc.ChangeStatus(context.Background(), statusAuthorizationActor(tc.roles...), "contract-1", 3, contract.StatusPendingPay, "业务已完成")
			if repo.transitions != 0 || repo.contract.Status != contract.StatusInProgress || repo.contract.Version != 3 {
				t.Fatalf("approval must not directly mutate contract: writes=%d contract=%+v", repo.transitions, repo.contract)
			}
			if !tc.allowed {
				if !errors.Is(err, ErrForbidden) || personnel.calls != 0 || len(temporal.Calls) != 0 {
					t.Fatalf("unauthorized approval: err=%v personnel_calls=%d temporal_calls=%d", err, personnel.calls, len(temporal.Calls))
				}
				return
			}
			if err != nil || result.ApprovalID == "" || result.WorkflowID != "workflow-1" || result.RunID != "run-1" || personnel.calls != 1 || len(temporal.Calls) != 1 {
				t.Fatalf("authorized approval: result=%+v err=%v personnel_calls=%d temporal_calls=%d", result, err, personnel.calls, len(temporal.Calls))
			}
			call := temporal.Calls[0]
			options := call.Arguments.Get(1).(client.StartWorkflowOptions)
			input := call.Arguments.Get(3).(workflows.StatusChangeInput)
			if options.ID != "status-change:tenant-1:contract-1:v3" || input.TenantID != "tenant-1" || input.ContractID != "contract-1" || input.ContractVersion != 3 || input.ApplicantUserID != "sales-1" || input.FromStatus != contract.StatusInProgress || input.TargetStatus != contract.StatusPendingPay || len(input.AdminUserIDs) != 1 || input.AdminUserIDs[0] != "admin-1" {
				t.Fatalf("incorrect approval request: options=%+v input=%+v", options, input)
			}
		})
	}
}

func TestChangeStatusPreservesValidationForAuthorizedOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version uint64
		target  contract.Status
		reason  string
		want    error
	}{
		{"stale_version", 2, contract.StatusCompleted, "业务已完成", apperrors.ErrVersionConflict},
		{"invalid_transition", 3, contract.StatusDraft, "业务已完成", contract.ErrInvalidTransition},
		{"empty_reason", 3, contract.StatusCompleted, "", ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &statusAuthorizationRepository{recordingRepository: &recordingRepository{contract: statusAuthorizationContract("tenant-1", "sales-1", contract.StatusInProgress)}}
			svc := &Service{Repo: repo}
			_, err := svc.ChangeStatus(context.Background(), statusAuthorizationActor("sales"), "contract-1", tc.version, tc.target, tc.reason)
			if !errors.Is(err, tc.want) || repo.transitions != 0 {
				t.Fatalf("err=%v writes=%d; want %v and no write", err, repo.transitions, tc.want)
			}
		})
	}
}

func TestSalesReadStillRejectsOtherOwnerWithBroadClaims(t *testing.T) {
	for _, tc := range []struct {
		name, tenant, owner string
		roles               []string
		allowed             bool
	}{
		{"sales_own", "tenant-1", "sales-1", []string{"sales"}, true},
		{"sales_other", "tenant-1", "sales-2", []string{"sales"}, false},
		{"sales_cross_tenant", "tenant-2", "sales-1", []string{"sales"}, false},
		{"admin_other", "tenant-1", "sales-2", []string{"admin"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &recordingRepository{contract: statusAuthorizationContract(tc.tenant, tc.owner, contract.StatusInProgress)}
			svc := &Service{Repo: repo}
			got, err := svc.GetContract(context.Background(), statusAuthorizationActor(tc.roles...), "contract-1")
			if !tc.allowed {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("read other contract: err=%v; want ErrForbidden", err)
				}
				return
			}
			if err != nil || got.ID != "contract-1" {
				t.Fatalf("authorized read: contract=%+v err=%v", got, err)
			}
		})
	}
}
