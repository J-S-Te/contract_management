package mysql

import (
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
)

func ordinaryLinkTarget(status contract.Status, customer uint64) contractRecord {
	number := "HT-LINK-" + ulid.Make().String()[:8]
	record := contractRecord{
		ID: ulid.Make().String(), TenantID: ulid.Make().String(), ContractNumber: &number,
		Status: string(status), OwnerUserID: ulid.Make().String(),
	}
	if customer != 0 {
		record.CRMCustomerID = &customer
	}
	return record
}

func linkIntake(customer uint64) opportunityIntakeRecord {
	return linkIntakeFor(customer, 7)
}

func linkIntakeFor(customer, opportunity uint64) opportunityIntakeRecord {
	return opportunityIntakeRecord{
		IntakeID: ulid.Make().String(), TenantID: ulid.Make().String(), EventID: ulid.Make().String(),
		OpportunityID: opportunity, CustomerID: customer, ContractRef: "ref", Status: application.OpportunityIntakeAccepted,
	}
}

// linkedLinkTarget 构造已关联指定商机的普通合同行；opportunity 为空表示未关联。
func linkedLinkTarget(status contract.Status, customer uint64, opportunity string) contractRecord {
	record := ordinaryLinkTarget(status, customer)
	if opportunity != "" {
		linked := opportunity
		record.OpportunityID = &linked
	}
	return record
}

func TestValidateOpportunityLinkTargetAllowsOnlyDraftOrPendingFirstAssociation(t *testing.T) {
	for _, status := range []contract.Status{contract.StatusDraft, contract.StatusPending} {
		target := ordinaryLinkTarget(status, 0)
		if err := validateOpportunityLinkTarget(target, linkIntake(42)); err != nil {
			t.Fatalf("%s 普通合同首次关联应允许: %v", status, err)
		}
	}
	// 已有相同客户时，普通合同允许补齐/重放同一商机关联。
	target := ordinaryLinkTarget(contract.StatusDraft, 42)
	if err := validateOpportunityLinkTarget(target, linkIntake(42)); err != nil {
		t.Fatalf("同客户普通合同应允许: %v", err)
	}
}

func TestValidateOpportunityLinkTargetRejectsImportedArchivedAndTerminated(t *testing.T) {
	imported := ordinaryLinkTarget(contract.StatusDraft, 0)
	imported.OriginKind = "IMPORTED"
	imported.ImportMode = "BASELINE"
	cases := []struct {
		name   string
		target contractRecord
	}{
		{name: "imported", target: imported},
		{name: "archived", target: ordinaryLinkTarget(contract.StatusArchived, 42)},
		{name: "terminated", target: ordinaryLinkTarget(contract.StatusTerminated, 42)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateOpportunityLinkTarget(testCase.target, linkIntake(42))
			if !errors.Is(err, application.ErrOpportunityLinkRejected) {
				t.Fatalf("err=%v want ErrOpportunityLinkRejected", err)
			}
		})
	}
}

func TestValidateOpportunityLinkTargetRejectsCustomerMismatchAndEffectiveFirstAssociation(t *testing.T) {
	// 已归属其它客户：任何状态都拒绝，必须走受控更正。
	if err := validateOpportunityLinkTarget(ordinaryLinkTarget(contract.StatusDraft, 99), linkIntake(42)); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("customer mismatch err=%v", err)
	}
	// 已审批/生效合同补建客户会改写已投递事实，同样拒绝。
	for _, status := range []contract.Status{contract.StatusApproved, contract.StatusActive, contract.StatusInProgress, contract.StatusPendingPay, contract.StatusCompleted} {
		if err := validateOpportunityLinkTarget(ordinaryLinkTarget(status, 0), linkIntake(42)); !errors.Is(err, application.ErrOpportunityLinkRejected) {
			t.Fatalf("%s 补建客户应拒绝: %v", status, err)
		}
	}
	// 已审批合同且客户一致时只补商机号，不改变客户归属，允许。
	if err := validateOpportunityLinkTarget(ordinaryLinkTarget(contract.StatusApproved, 42), linkIntake(42)); err != nil {
		t.Fatalf("approved same customer err=%v", err)
	}
	// 缺失客户接入不能建立归属。
	if err := validateOpportunityLinkTarget(ordinaryLinkTarget(contract.StatusDraft, 0), linkIntake(0)); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("zero intake customer err=%v", err)
	}
}

func TestValidateOpportunityLinkTargetRejectsOpportunityRewriteWithinSameCustomer(t *testing.T) {
	// 合同已关联商机 7，同客户商机 8 的核对不能静默改写为另一商机。
	if err := validateOpportunityLinkTarget(linkedLinkTarget(contract.StatusDraft, 42, "7"), linkIntakeFor(42, 8)); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("same customer different opportunity err=%v want ErrOpportunityLinkRejected", err)
	}
	// 已审批合同在同客户下改写为另一商机同样必须拒绝。
	if err := validateOpportunityLinkTarget(linkedLinkTarget(contract.StatusApproved, 42, "7"), linkIntakeFor(42, 8)); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("approved different opportunity err=%v want ErrOpportunityLinkRejected", err)
	}
	// 同一商机重复确认保持幂等放行。
	for _, status := range []contract.Status{contract.StatusDraft, contract.StatusPending, contract.StatusApproved} {
		if err := validateOpportunityLinkTarget(linkedLinkTarget(status, 42, "7"), linkIntakeFor(42, 7)); err != nil {
			t.Fatalf("%s same opportunity replay err=%v", status, err)
		}
	}
	// 未关联商机（空值/仅空白）视为首次关联，保持既有放行。
	for _, empty := range []string{"", "   "} {
		if err := validateOpportunityLinkTarget(linkedLinkTarget(contract.StatusDraft, 42, empty), linkIntakeFor(42, 8)); err != nil {
			t.Fatalf("empty opportunity %q err=%v", empty, err)
		}
	}
	// 客户不一致优先按客户门禁拒绝，提示保持“归属其他 CRM 客户”。
	if err := validateOpportunityLinkTarget(linkedLinkTarget(contract.StatusDraft, 99, "7"), linkIntakeFor(42, 8)); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("customer mismatch err=%v", err)
	}
}

func TestResolveOpportunityLinkTargetPrefersIDAndRejectsDivergence(t *testing.T) {
	byID := ordinaryLinkTarget(contract.StatusDraft, 0)
	byNumber := ordinaryLinkTarget(contract.StatusDraft, 0)
	if resolved, err := resolveOpportunityLinkTarget(&byID, nil); err != nil || resolved.ID != byID.ID {
		t.Fatalf("id only resolved=%+v err=%v", resolved, err)
	}
	if resolved, err := resolveOpportunityLinkTarget(nil, &byNumber); err != nil || resolved.ID != byNumber.ID {
		t.Fatalf("number only resolved=%+v err=%v", resolved, err)
	}
	if resolved, err := resolveOpportunityLinkTarget(&byID, &byID); err != nil || resolved.ID != byID.ID {
		t.Fatalf("same target resolved=%+v err=%v", resolved, err)
	}
	if _, err := resolveOpportunityLinkTarget(&byID, &byNumber); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("divergence err=%v", err)
	}
	if _, err := resolveOpportunityLinkTarget(nil, nil); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("missing err=%v", err)
	}
}
