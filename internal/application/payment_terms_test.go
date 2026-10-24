package application

import (
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func TestPaymentPlanDraftRetainsOmittedScheduleAndRejectsChangedTotal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		amount   int64
		currency string
		clear    bool
		want     error
	}{
		{"preserve", 1000000, "CNY", false, nil},
		{"amount changed without schedule", 2000000, "CNY", false, ErrValidation},
		{"currency changed without schedule", 1000000, "USD", false, ErrValidation},
		{"explicit draft clear", 1000000, "CNY", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, actor, input := draftFixture(t)
			s.CRMContractReferences = validCRMReference(7)
			input.CRMCustomerID = 7
			repo.contract.AmountMinor, repo.contract.Currency = 1000000, "CNY"
			repo.contract.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "10000.00"}}
			input.AmountMinor, input.Currency = tc.amount, tc.currency
			if tc.clear {
				input.PaymentTerms = []contract.PaymentTerm{}
			}
			got, err := s.UpdateContractDraft(context.Background(), actor, "C", 3, input)
			if !errors.Is(err, tc.want) {
				t.Fatal(err)
			}
			if tc.want != nil {
				if repo.writes != 0 {
					t.Fatal("invalid schedule wrote draft")
				}
				return
			}
			if tc.clear && len(got.PaymentTerms) != 0 || !tc.clear && len(got.PaymentTerms) != 1 || got.Version != 4 {
				t.Fatal("schedule/version not preserved")
			}
		})
	}
}

func TestPaymentPlanCannotEditAnotherOwnerOrPendingApproval(t *testing.T) {
	for _, pending := range []bool{false, true} {
		s, repo, actor, input := draftFixture(t)
		s.CRMContractReferences = validCRMReference(7)
		input.CRMCustomerID = 7
		input.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "10000.00"}}
		want := ErrForbidden
		if pending {
			repo.contract.Status = contract.StatusPending
			want = apperrors.ErrStateConflict
		} else {
			actor.UserID = "other"
		}
		if _, err := s.UpdateContractDraft(context.Background(), actor, "C", 3, input); !errors.Is(err, want) || repo.writes != 0 {
			t.Fatalf("financial scope/state bypass: %v writes=%d", err, repo.writes)
		}
	}
}
