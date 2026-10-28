package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
)

// TestResponsibilityTransactionsOnIsolatedMySQL 验证交接、撤销、失败回滚和重试状态真实持久化。
// 参数：t 为测试上下文；仅显式CONTRACT_TEST_DSN启用隔离库。
// 返回值：无。
func TestResponsibilityTransactionsOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	oldOwner, newOwner := ulid.Make().String(), ulid.Make().String()
	id := seedHandoverContract(t, r.db, tenant, oldOwner, "active")
	t.Cleanup(func() { _ = r.db.Where("tenant_id=?", tenant).Delete(&handoverReceiptRecord{}).Error })
	if err := r.db.Transaction(func(tx *gorm.DB) error { return enqueueContractResponsibility(tx, tenant, id) }); err != nil {
		t.Fatal(err)
	}
	in := application.HandoverTransferInput{TenantID: tenant, ResourceID: id, ResourceType: "contract", FromUserID: oldOwner, ToUserID: newOwner, ExpectedVersion: 3, IdempotencyKey: "handover:" + id, BatchID: ulid.Make().String(), ItemID: ulid.Make().String()}
	blocker := errors.New("isolated responsibility outbox failure")
	if err := r.db.Callback().Create().Before("gorm:create").Register("test:responsibility_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "con_contract_responsibility_outbox" {
			tx.AddError(blocker)
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, err := r.TransferOwnership(ctx, in, time.Now())
	_ = r.db.Callback().Create().Remove("test:responsibility_failure")
	if !errors.Is(err, blocker) {
		t.Fatalf("Outbox失败未阻断交接：%v", err)
	}
	var current contractRecord
	if err = r.db.Where("tenant_id=? AND id=?", tenant, id).Take(&current).Error; err != nil || current.OwnerUserID != oldOwner || current.Version != 3 {
		t.Fatalf("失败事务残留负责人变化：%+v %v", current, err)
	}
	if _, err = r.TransferOwnership(ctx, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = r.TransferOwnership(ctx, in, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = r.db.Transaction(func(tx *gorm.DB) error {
		return updateStatus(tx, tenant, id, contract.StatusActive, contract.StatusTerminated, newOwner)
	}); err != nil {
		t.Fatal(err)
	}
	var events []contractResponsibilityOutbox
	if err = r.db.Where("tenant_id=? AND contract_id=?", tenant, id).Order("contract_version").Find(&events).Error; err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].ContractVersion != 1 || events[1].ContractVersion != 2 || events[2].ContractVersion != 3 {
		t.Fatalf("独立责任版本错误：%+v", events)
	}
	var stream contractResponsibilityStream
	if err = r.db.Where("tenant_id=? AND contract_id=?", tenant, id).Take(&stream).Error; err != nil || stream.Active || stream.OwnerUserID != newOwner {
		t.Fatalf("终态未撤销：%+v %v", stream, err)
	}
	target := events[2]
	if err = r.db.Model(&contractResponsibilityOutbox{}).Where("id=?", target.ID).Update("delivery_status", "processing").Error; err != nil {
		t.Fatal(err)
	}
	if err = r.MarkContractResponsibilityFailed(ctx, target.ID, "temporary receiver failure", 2, false); err != nil {
		t.Fatal(err)
	}
	var retried contractResponsibilityOutbox
	if err = r.db.Where("id=?", target.ID).Take(&retried).Error; err != nil || retried.DeliveryStatus != "pending" || retried.LockedAt != nil || !retried.NextAttemptAt.After(target.NextAttemptAt) {
		t.Fatalf("重试状态未持久化：%+v %v", retried, err)
	}
}

// TestResponsibilityCreationIsAtomicOnMySQL 验证创建时真实负责人及Outbox必须同事务成功。
// 参数：t 为测试上下文；仅使用显式隔离CONTRACT_TEST_DSN。
// 返回值：无。
func TestResponsibilityCreationIsAtomicOnMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: "responsibility", Type: "sales", ServiceType: "integration", OwnerUserID: ulid.Make().String(), Currency: "CNY", AmountMinor: 100, Content: "test", Status: contract.StatusDraft}
	blocker := errors.New("isolated outbox failure")
	if err := r.db.Callback().Create().Before("gorm:create").Register("test:responsibility_create_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "con_contract_responsibility_outbox" {
			tx.AddError(blocker)
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := r.CreateContract(ctx, c, "creator")
	_ = r.db.Callback().Create().Remove("test:responsibility_create_failure")
	if !errors.Is(err, blocker) {
		t.Fatalf("责任Outbox失败未回滚创建：%v", err)
	}
	var count int64
	if err = r.db.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, c.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("失败创建残留合同：%d %v", count, err)
	}
	if err = r.CreateContract(ctx, c, "creator"); err != nil {
		t.Fatal(err)
	}
	var stream contractResponsibilityStream
	if err = r.db.Where("tenant_id=? AND contract_id=?", tenant, c.ID).Take(&stream).Error; err != nil || stream.OwnerUserID != c.OwnerUserID || stream.Version != 1 || !stream.Active {
		t.Fatalf("创建责任不是真实owner：%+v %v", stream, err)
	}
}
