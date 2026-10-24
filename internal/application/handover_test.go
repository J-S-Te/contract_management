package application

import (
	"context"
	"errors"
	"testing"
	"time"
)

type handoverRepositoryFake struct {
	freezes          map[string]HandoverFreeze
	responsibilities []HandoverResponsibility
	receipts         map[string]HandoverReceipt
	transferCalls    int
	released         []string
}

func newHandoverRepositoryFake(resources ...string) *handoverRepositoryFake {
	fake := &handoverRepositoryFake{freezes: map[string]HandoverFreeze{}, receipts: map[string]HandoverReceipt{}}
	for _, resource := range resources {
		fake.responsibilities = append(fake.responsibilities, HandoverResponsibility{
			ResourceType: "contract", ResourceID: resource, ResourceName: resource,
			OwnerUserID: "user-leaver", Version: 3, UpdatedAt: time.Now().UTC(),
		})
	}
	return fake
}

func (fake *handoverRepositoryFake) CreateFreeze(_ context.Context, freeze HandoverFreeze) (HandoverFreeze, bool, error) {
	key := freeze.TenantID + "|" + freeze.BatchID + "|" + freeze.UserID
	if existing, ok := fake.freezes[key]; ok {
		return existing, false, nil
	}
	fake.freezes[key] = freeze
	return freeze, true, nil
}

func (fake *handoverRepositoryFake) ReleaseFreeze(_ context.Context, tenantID, batchID, outcome string, now time.Time) error {
	fake.released = append(fake.released, tenantID+"|"+batchID+"|"+outcome)
	for key, freeze := range fake.freezes {
		if freeze.BatchID == batchID {
			freeze.Status = HandoverFreezeReleased
			freeze.ReleasedAt = &now
			freeze.Outcome = outcome
			fake.freezes[key] = freeze
		}
	}
	return nil
}

func (fake *handoverRepositoryFake) ActiveFreeze(_ context.Context, tenantID, userID string) (HandoverFreeze, bool, error) {
	for _, freeze := range fake.freezes {
		if freeze.TenantID == tenantID && freeze.UserID == userID && freeze.Status == HandoverFreezeFrozen {
			return freeze, true, nil
		}
	}
	return HandoverFreeze{}, false, nil
}

func (fake *handoverRepositoryFake) ListOwnedResponsibilities(_ context.Context, _, userID, cursor string, limit int) ([]HandoverResponsibility, error) {
	items := []HandoverResponsibility{}
	for _, item := range fake.responsibilities {
		if item.OwnerUserID != userID {
			continue
		}
		if cursor != "" && item.ResourceID <= cursor {
			continue
		}
		items = append(items, item)
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func (fake *handoverRepositoryFake) TransferOwnership(_ context.Context, input HandoverTransferInput, now time.Time) (HandoverReceipt, error) {
	fake.transferCalls++
	receipt := HandoverReceipt{
		ItemID: input.ItemID, IdempotencyKey: input.IdempotencyKey, Status: HandoverReceiptApplied,
		AppliedVersion: input.ExpectedVersion + 1, EvidenceRef: "audit:" + input.ResourceID, ReceivedAt: now,
	}
	fake.receipts[input.TenantID+"|"+input.IdempotencyKey] = receipt
	return receipt, nil
}

func (fake *handoverRepositoryFake) SaveReceipt(_ context.Context, receipt HandoverReceipt) error {
	fake.receipts[receipt.TenantID+"|"+receipt.IdempotencyKey] = receipt
	return nil
}

func (fake *handoverRepositoryFake) FindReceipt(_ context.Context, tenantID, idempotencyKey string) (HandoverReceipt, bool, error) {
	receipt, ok := fake.receipts[tenantID+"|"+idempotencyKey]
	return receipt, ok, nil
}

func handoverServiceFor(t *testing.T, repository *handoverRepositoryFake) *HandoverService {
	t.Helper()
	counter := 0
	service, err := NewHandoverService(repository, func(time.Time) (string, error) {
		counter++
		return "01HANDOVER" + string(rune('A'+counter%26)) + "000000000000000000", nil
	}, func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestHandoverFreezeIsIdempotentAndReportsResponsibilities(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1", "c-2", "c-3")
	service := handoverServiceFor(t, repository)
	ctx := context.Background()

	first, count, err := service.Freeze(ctx, "tenant-1", "batch-1", "request-1", "user-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != HandoverFreezeFrozen || count != 3 {
		t.Fatalf("freeze = %+v count = %d", first, count)
	}
	second, count, err := service.Freeze(ctx, "tenant-1", "batch-1", "request-1", "user-leaver")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || count != 3 {
		t.Fatalf("repeated freeze must be idempotent: %+v count=%d", second, count)
	}
	if _, _, err := service.Freeze(ctx, "tenant-1", "batch-1", "request-1", ""); !errors.Is(err, ErrHandoverValidation) {
		t.Fatalf("missing user must be rejected: %v", err)
	}
}

func TestHandoverResponsibilitiesPaginateWithCategory(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1", "c-2", "c-3")
	service := handoverServiceFor(t, repository)

	items, next, err := service.Responsibilities(context.Background(), "tenant-1", "user-leaver", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || next != "c-2" {
		t.Fatalf("page = %d next = %q", len(items), next)
	}
	if items[0].Category != HandoverCategoryContractOwner || items[0].ResourceType != "contract" {
		t.Fatalf("responsibility not tagged as contract ownership: %+v", items[0])
	}
	// 非本人责任不返回。
	if other, _, err := service.Responsibilities(context.Background(), "tenant-1", "user-other", "", 10); err != nil || len(other) != 0 {
		t.Fatalf("other users' responsibilities leaked: %d (%v)", len(other), err)
	}
}

func TestHandoverTransferIsIdempotentAndRejectsSelfSuccessor(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	service := handoverServiceFor(t, repository)
	ctx := context.Background()

	input := HandoverTransferInput{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", ResourceType: "contract", ResourceID: "c-1",
		FromUserID: "user-leaver", ToUserID: "user-successor", ExpectedVersion: 3, IdempotencyKey: "handover:batch-1:item-1",
	}
	receipt, err := service.Transfer(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != HandoverReceiptApplied || receipt.AppliedVersion != 4 {
		t.Fatalf("receipt = %+v", receipt)
	}
	if _, err := service.Transfer(ctx, input); err != nil {
		t.Fatalf("replay must succeed: %v", err)
	}
	if repository.transferCalls != 1 {
		t.Fatalf("transfer executed %d times, want exactly once", repository.transferCalls)
	}

	self := input
	self.IdempotencyKey = "handover:batch-1:item-2"
	self.ToUserID = self.FromUserID
	if _, err := service.Transfer(ctx, self); !errors.Is(err, ErrHandoverValidation) {
		t.Fatalf("self-successor must be rejected: %v", err)
	}
}

func TestHandoverTransferRejectsFrozenSuccessor(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	repository.freezes["tenant-1|batch-9|user-successor"] = HandoverFreeze{
		ID: "freeze-9", TenantID: "tenant-1", BatchID: "batch-9", UserID: "user-successor", Status: HandoverFreezeFrozen,
	}
	service := handoverServiceFor(t, repository)

	_, err := service.Transfer(context.Background(), HandoverTransferInput{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", ResourceID: "c-1",
		FromUserID: "user-leaver", ToUserID: "user-successor", ExpectedVersion: 3, IdempotencyKey: "handover:batch-1:item-1",
	})
	if !errors.Is(err, ErrHandoverFrozen) {
		t.Fatalf("err = %v, want ErrHandoverFrozen", err)
	}
	if repository.transferCalls != 0 {
		t.Fatal("transfer must not run when the successor is frozen")
	}
}

func TestHandoverReceiptLookupAndCompletion(t *testing.T) {
	repository := newHandoverRepositoryFake("c-1")
	service := handoverServiceFor(t, repository)
	ctx := context.Background()

	if _, err := service.Receipt(ctx, "tenant-1", "missing-key"); !errors.Is(err, ErrHandoverNotFound) {
		t.Fatalf("unknown receipt must be reported: %v", err)
	}
	if _, err := service.Transfer(ctx, HandoverTransferInput{
		TenantID: "tenant-1", BatchID: "batch-1", ItemID: "item-1", ResourceID: "c-1",
		FromUserID: "user-leaver", ToUserID: "user-successor", ExpectedVersion: 3, IdempotencyKey: "handover:batch-1:item-1",
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := service.Receipt(ctx, "tenant-1", "handover:batch-1:item-1")
	if err != nil || receipt.Status != HandoverReceiptApplied {
		t.Fatalf("receipt = %+v (%v)", receipt, err)
	}
	if err := service.Complete(ctx, "tenant-1", "batch-1", "COMPLETED"); err != nil {
		t.Fatal(err)
	}
	if len(repository.released) != 1 || repository.released[0] != "tenant-1|batch-1|COMPLETED" {
		t.Fatalf("released = %v", repository.released)
	}
	if err := service.Complete(ctx, "tenant-1", "batch-1", "SOMETHING"); !errors.Is(err, ErrHandoverValidation) {
		t.Fatalf("invalid outcome must be rejected: %v", err)
	}
}
