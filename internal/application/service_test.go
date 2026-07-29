package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

type recordingRepository struct {
	ownerUserID string
	contract    contract.Contract
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
	return approval.Meta{}, nil
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
