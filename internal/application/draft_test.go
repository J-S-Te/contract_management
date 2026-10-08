package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

type draftMemoryRepository struct {
	recordingRepository
	writes int
}

func (r *draftMemoryRepository) UpdateContractDraft(_ context.Context, c contract.Contract, v uint64, actor string) error {
	if r.contract.Version != v {
		return apperrors.ErrVersionConflict
	}
	if r.contract.Status != contract.StatusDraft {
		return apperrors.ErrStateConflict
	}
	r.writes++
	c.Version = v + 1
	r.contract = c
	return nil
}
func draftFixture(t *testing.T) (*Service, *draftMemoryRepository, Principal, contract.Contract) {
	t.Helper()
	current := contract.Contract{ID: "C", TenantID: "tenant", TemplateID: "template", CreatedBy: "sales-user", OwnerUserID: "sales-user", OwnerIdentityID: "identity", Status: contract.StatusDraft, Version: 3}
	repo := &draftMemoryRepository{recordingRepository: recordingRepository{contract: current}}
	actor := Principal{TenantID: "tenant", UserID: "sales-user", IdentityID: "identity", Roles: []string{"sales"}, Permissions: map[string]bool{"contract.edit": true}, PermissionScopes: allowAllScope("contract.edit")}
	service := &Service{Repo: repo, Templates: &memoryTemplateRepository{items: map[string]contracttemplate.Template{"template": {ID: "template", TenantID: "tenant", Content: applicationTestDOCX(t, "{{合同金额}}"), Fields: []contracttemplate.Field{{Name: "合同金额", Label: "合同金额"}}}}}}
	input := contract.Contract{TemplateID: "template", Title: "Edited", Type: "直签", AmountMinor: 1000000, ServiceItems: []contract.ServiceItem{{ServiceType: "软件测试"}}, TemplateValues: map[string]string{"合同金额": "1"}}
	return service, repo, actor, input
}
func TestUpdateDraftReusesAmountRenderingAndPreservesOwnership(t *testing.T) {
	s, r, a, in := draftFixture(t)
	got, err := s.UpdateContractDraft(context.Background(), a, "C", 3, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 4 || got.TemplateValues["合同金额"] != "10000.00" || got.OwnerUserID != "sales-user" || got.CreatedBy != "sales-user" || !got.CanEditDraft || r.writes != 1 {
		t.Fatalf("unexpected update: %+v", got)
	}
	if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, in); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale update: %v", err)
	}
}
func TestUpdateDraftAuthorizationAndState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*draftMemoryRepository, *Principal, *contract.Contract)
		want   error
	}{
		{"other sales", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) { a.UserID = "other" }, ErrForbidden},
		{"director", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) {
			a.Roles = []string{"sales_director"}
		}, ErrForbidden},
		{"no edit permission", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) { a.Permissions = nil }, ErrForbidden},
		{"pending", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) {
			r.contract.Status = contract.StatusPending
		}, apperrors.ErrStateConflict},
		{"source change", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) { in.TemplateID = "other" }, ErrValidation},
		{"forged number", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) { in.Number = "fake" }, ErrValidation},
		{"foreign service item", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) {
			in.ServiceItems[0].SourceID = "other-contract:item"
		}, ErrValidation},
		{"owner is not creator", func(r *draftMemoryRepository, a *Principal, in *contract.Contract) {
			r.contract.CreatedBy = "other-creator"
		}, ErrForbidden},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s, r, a, in := draftFixture(t)
			tt.mutate(r, &a, &in)
			_, err := s.UpdateContractDraft(context.Background(), a, "C", 3, in)
			if !errors.Is(err, tt.want) || r.writes != 0 {
				t.Fatalf("error %v writes %d", err, r.writes)
			}
		})
	}
	s, _, a, in := draftFixture(t)
	a.UserID = "admin-user"
	a.Roles = []string{"admin"}
	if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, in); err != nil {
		t.Fatalf("admin update: %v", err)
	}
}

func TestExternalDraftEditPreservesOriginalDocument(t *testing.T) {
	s, r, a, in := draftFixture(t)
	r.contract.TemplateID = ""
	r.contract.AmountMinor = in.AmountMinor
	r.contract.Currency = "CNY"
	in.Currency = "CNY"
	r.contract.Document = applicationTestDOCX(t, "original external contract")
	r.contract.Content = "original external contract"
	r.contract.SourceFileID = "source-file"
	r.contract.SourceFileStatus = "READY"
	in.TemplateID = ""
	in.TemplateValues = nil
	in.Content = "original external contract"
	in.CRMCustomerID = 9
	in.CustomerName = "client"
	in.ServiceItems = []contract.ServiceItem{{ServiceType: "软件测试", Site: "现场", Batch: "第一批次", Category: "软件测试", TestMode: "STANDARD"}}
	s.CRMContractReferences = validCRMReference(9)
	s.DetectionCategories = enabledDetectionCategories("软件测试")
	changedAmount := in
	changedAmount.AmountMinor++
	if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, changedAmount); !errors.Is(err, ErrValidation) {
		t.Fatalf("external amount edit: %v", err)
	}
	changedCurrency := in
	changedCurrency.Currency = "USD"
	if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, changedCurrency); !errors.Is(err, ErrValidation) {
		t.Fatalf("external currency edit: %v", err)
	}
	changedBody := in
	changedBody.Content = "replacement"
	if _, err := s.UpdateContractDraft(context.Background(), a, "C", 3, changedBody); !errors.Is(err, ErrValidation) {
		t.Fatalf("external body edit: %v", err)
	}
	old := string(r.contract.Document)
	got, err := s.UpdateContractDraft(context.Background(), a, "C", 3, in)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Document) != old || got.Content != "original external contract" || got.SourceFileID != "source-file" || got.CustomerName != "CRM 权威客户" {
		t.Fatal("external edit replaced source or skipped CRM validation")
	}
}

func TestSubmitDraftChecksVersionAndCreatorBeforeStartingWorkflow(t *testing.T) {
	s, r, a, _ := draftFixture(t)
	a.Permissions["contract.create"] = true
	a.PermissionScopes["contract.create"] = contract.ScopeFilter{AllowAll: true}
	if _, err := s.SubmitContractVersion(context.Background(), a, "C", true, 2); !errors.Is(err, apperrors.ErrVersionConflict) {
		t.Fatalf("stale submit %v", err)
	}
	r.contract.CreatedBy = "other"
	if _, err := s.SubmitContractVersion(context.Background(), a, "C", true, 3); !errors.Is(err, ErrForbidden) {
		t.Fatalf("foreign submit %v", err)
	}
}
