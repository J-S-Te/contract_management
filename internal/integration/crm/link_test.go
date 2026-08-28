package crm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

func TestEncodeOpportunityLinkCallbackUsesStableCRMContract(t *testing.T) {
	t.Parallel()
	reviewedAt := time.Date(2026, time.August, 27, 10, 30, 0, 0, time.UTC)
	payload, err := EncodeOpportunityLinkCallback(application.OpportunityIntake{
		EventID: "event-1", IntakeID: "intake-1", OpportunityID: 42,
		ContractID: "01J00000000000000000000000", ContractNumber: "HT-2026-001",
		Status: application.OpportunityIntakeLinkConfirmed, Version: 2, ReviewedAt: &reviewedAt,
	})
	if err != nil {
		t.Fatalf("encode callback: %v", err)
	}
	var callback OpportunityLinkCallback
	if err := json.Unmarshal(payload, &callback); err != nil {
		t.Fatalf("decode callback: %v", err)
	}
	if callback.OpportunityID != 42 || callback.SyncVersion != 2 || callback.LinkedAt == nil || !callback.LinkedAt.Equal(reviewedAt) {
		t.Fatalf("callback = %#v", callback)
	}
}

func TestEncodeOpportunityLinkExceptionFallsBackToContractReference(t *testing.T) {
	t.Parallel()
	reviewedAt := time.Now().UTC()
	payload, err := EncodeOpportunityLinkCallback(application.OpportunityIntake{
		EventID: "event-2", IntakeID: "intake-2", OpportunityID: 43,
		ContractRef: "HT-2026-002", Status: application.OpportunityIntakeLinkException,
		Version: 2, ReviewedAt: &reviewedAt,
	})
	if err != nil {
		t.Fatalf("encode callback: %v", err)
	}
	var callback OpportunityLinkCallback
	if err := json.Unmarshal(payload, &callback); err != nil {
		t.Fatalf("decode callback: %v", err)
	}
	if callback.ContractNumber != "HT-2026-002" || callback.LinkedAt != nil || callback.ContractID != "" {
		t.Fatalf("callback = %#v", callback)
	}
}
