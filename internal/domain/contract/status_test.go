package contract

import "testing"

func TestLifecycleTransitions(t *testing.T) {
	t.Parallel()
	valid := [][2]Status{
		{StatusDraft, StatusPending}, {StatusPending, StatusApproved},
		{StatusApproved, StatusActive}, {StatusActive, StatusInProgress},
		{StatusInProgress, StatusPendingPay}, {StatusPendingPay, StatusCompleted},
		{StatusCompleted, StatusArchived}, {StatusTerminated, StatusArchived},
	}
	for _, pair := range valid {
		if err := ValidateTransition(pair[0], pair[1]); err != nil {
			t.Fatalf("expected %s -> %s to be valid: %v", pair[0], pair[1], err)
		}
	}
	invalid := [][2]Status{{StatusDraft, StatusActive}, {StatusArchived, StatusDraft}, {StatusCompleted, StatusPendingPay}}
	for _, pair := range invalid {
		if err := ValidateTransition(pair[0], pair[1]); err == nil {
			t.Fatalf("expected %s -> %s to be invalid", pair[0], pair[1])
		}
	}
}

func TestApprovalPassedExcludesDraftAndPending(t *testing.T) {
	if StatusDraft.ApprovalPassed() || StatusPending.ApprovalPassed() {
		t.Fatal("unapproved status was visible to contract specialist")
	}
	for _, status := range ApprovalPassedStatuses() {
		if !status.ApprovalPassed() {
			t.Fatalf("%s should be approval-passed", status)
		}
	}
}

func TestCriticalTargetsRequireApproval(t *testing.T) {
	t.Parallel()
	for _, target := range []Status{StatusInProgress, StatusPendingPay, StatusTerminated, StatusArchived} {
		if !target.RequiresApproval() {
			t.Fatalf("expected %s to require approval", target)
		}
	}
}
