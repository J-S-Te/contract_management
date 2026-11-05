package contract

import "testing"

func TestLifecycleTransitions(t *testing.T) {
	t.Parallel()
	valid := [][2]Status{
		{StatusDraft, StatusPending}, {StatusPending, StatusApproved},
		// pending→draft 是审批撤回（CompleteApproval）落库所用的迁移，状态机必须保留；
		// 在途审批期间的直迁由仓储层在途计数拦截，不能靠删除该迁移实现。
		{StatusPending, StatusDraft},
		{StatusApproved, StatusActive}, {StatusActive, StatusInProgress},
		{StatusInProgress, StatusPendingPay}, {StatusPendingPay, StatusCompleted},
		{StatusCompleted, StatusArchived}, {StatusTerminated, StatusArchived},
	}
	for _, pair := range valid {
		if err := ValidateTransition(pair[0], pair[1]); err != nil {
			t.Fatalf("expected %s -> %s to be valid: %v", pair[0], pair[1], err)
		}
	}
	// 撤回迁移保留的同时，pending 也不是“可直接完成”的状态。
	if err := ValidateTransition(StatusPending, StatusCompleted); err == nil {
		t.Fatal("expected pending -> completed to remain invalid")
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
