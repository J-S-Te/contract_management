package application

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type handoverFreezeCheckerStub struct {
	freeze   HandoverFreeze
	found    bool
	err      error
	calls    int
	tenantID string
	userID   string
}

func (stub *handoverFreezeCheckerStub) ActiveFreeze(_ context.Context, tenantID, userID string) (HandoverFreeze, bool, error) {
	stub.calls++
	stub.tenantID, stub.userID = tenantID, userID
	return stub.freeze, stub.found, stub.err
}

// 被交接冻结的用户不得成为新合同责任人。
func TestEnsureOwnerNotFrozenRejectsFrozenOwner(t *testing.T) {
	checker := &handoverFreezeCheckerStub{
		found:  true,
		freeze: HandoverFreeze{ID: "freeze-1", BatchID: "batch-9", UserID: "user-leaver", Status: HandoverFreezeFrozen},
	}
	service := &Service{HandoverFreeze: checker}

	err := service.EnsureOwnerNotFrozen(context.Background(), "tenant-1", "user-leaver")
	if !errors.Is(err, ErrHandoverFrozen) {
		t.Fatalf("err = %v, want ErrHandoverFrozen", err)
	}
	if !strings.Contains(err.Error(), "batch-9") {
		t.Fatalf("error must name the blocking batch: %v", err)
	}
	if checker.tenantID != "tenant-1" || checker.userID != "user-leaver" {
		t.Fatalf("freeze lookup used %s/%s", checker.tenantID, checker.userID)
	}
}

// 非冻结用户正常放行；已解除的冻结不拦截。
func TestEnsureOwnerNotFrozenAllowsUnfrozenOwner(t *testing.T) {
	for name, checker := range map[string]*handoverFreezeCheckerStub{
		"no-freeze-record": {found: false},
		"released-freeze":  {found: true, freeze: HandoverFreeze{Status: HandoverFreezeReleased}},
	} {
		t.Run(name, func(t *testing.T) {
			service := &Service{HandoverFreeze: checker}
			if err := service.EnsureOwnerNotFrozen(context.Background(), "tenant-1", "user-other"); err != nil {
				t.Fatalf("unfrozen owner must be accepted: %v", err)
			}
		})
	}
}

// 未接线检查器时保持既有行为（向后兼容），空责任人也不查询。
func TestEnsureOwnerNotFrozenSkipsWhenUnwiredOrEmptyOwner(t *testing.T) {
	unwired := &Service{}
	if err := unwired.EnsureOwnerNotFrozen(context.Background(), "tenant-1", "user-1"); err != nil {
		t.Fatalf("unwired checker must not block: %v", err)
	}
	checker := &handoverFreezeCheckerStub{found: true, freeze: HandoverFreeze{Status: HandoverFreezeFrozen}}
	service := &Service{HandoverFreeze: checker}
	if err := service.EnsureOwnerNotFrozen(context.Background(), "tenant-1", "   "); err != nil {
		t.Fatalf("empty owner must be skipped: %v", err)
	}
	if checker.calls != 0 {
		t.Fatalf("empty owner must not trigger a freeze lookup, calls = %d", checker.calls)
	}
}

// 冻结查询失败必须显式上报（失败时安全拒绝），不能当成"未冻结"放行。
func TestEnsureOwnerNotFrozenReportsLookupFailure(t *testing.T) {
	checker := &handoverFreezeCheckerStub{err: errors.New("freeze store unavailable")}
	service := &Service{HandoverFreeze: checker}
	err := service.EnsureOwnerNotFrozen(context.Background(), "tenant-1", "user-1")
	if err == nil || errors.Is(err, ErrHandoverFrozen) || !strings.Contains(err.Error(), "freeze store unavailable") {
		t.Fatalf("err = %v, want a wrapped lookup failure", err)
	}
}
