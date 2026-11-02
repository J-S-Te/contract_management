package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/oklog/ulid/v2"
)

// TestIntegrationOutboxDeadListAndRetryOnIsolatedMySQL 验证四类集成死信的
// 可见性、重试状态重置与“可再次领取”的真实持久化语义。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestIntegrationOutboxDeadListAndRetryOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	contractID := seedHandoverContract(t, r.db, tenant, ulid.Make().String(), "active")
	intakeID := ulid.Make().String()
	linkID := ulid.Make().String()
	now := time.Now().UTC()
	intake := opportunityIntakeRecord{
		IntakeID: intakeID, TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: 7,
		EventVersion: 1, OpportunityNo: "SJ-DEAD", CustomerID: 42, ContractRef: contractID,
		ContractID: &contractID, ExpectedAmount: "100.00", OccurredAt: now,
		Status: application.OpportunityIntakeLinkConfirmed, Version: 2, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.db.Create(&intake).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 死信与接入记录不在通用隔离清理列表内，按依赖顺序显式清理本测试租户。
		for _, model := range []any{&opportunityLinkOutboxRecord{}, &opportunityIntakeReviewRecord{}, &opportunityIntakeRecord{}, &auditOutboxRecord{}} {
			if err := r.db.Where("tenant_id = ?", tenant).Delete(model).Error; err != nil {
				t.Error("integration outbox test cleanup failed:", err)
			}
		}
	})
	rows := []any{
		&financialEffectiveOutboxRecord{ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: 1, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
		&contractResponsibilityOutbox{ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: 2, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
		&projectDeliveryOutboxRecord{ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: 3, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
		&opportunityLinkOutboxRecord{ID: linkID, TenantID: tenant, IntakeID: intakeID, EventID: intake.EventID, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
	}
	for _, row := range rows {
		if err := r.db.Create(row).Error; err != nil {
			t.Fatalf("seed dead outbox row: %v", err)
		}
	}

	for _, kind := range application.IntegrationOutboxKinds() {
		items, err := r.ListFailedIntegrationEvents(ctx, tenant, kind, 10)
		if err != nil {
			t.Fatalf("%s list: %v", kind, err)
		}
		if len(items) != 1 || items[0].Kind != kind || items[0].AggregateID == "" || items[0].Event == "" || items[0].Attempts != 20 {
			t.Fatalf("%s items=%+v", kind, items)
		}
		found, err := r.RetryFailedIntegrationEvent(ctx, tenant, kind, items[0].ID, "ops-1")
		if err != nil || !found {
			t.Fatalf("%s retry found=%v err=%v", kind, found, err)
		}
		// 重试必须真的变回可领取状态（attempts 归零、清空 locked_at/last_error）。
		var status struct {
			DeliveryStatus string
			Attempts       uint
			LockedAt       *time.Time
			LastError      *string
		}
		model, _ := integrationOutboxModel(kind)
		if err := r.db.Model(model).Select("delivery_status, attempts, locked_at, last_error").Where("id = ?", items[0].ID).Take(&status).Error; err != nil {
			t.Fatal(err)
		}
		if status.DeliveryStatus != "pending" || status.Attempts != 0 || status.LockedAt != nil || status.LastError != nil {
			t.Fatalf("%s retried status=%+v", kind, status)
		}
	}
	// 再次列表应为空；重试幂等但 dead 已不存在时返回 found=false。
	if items, err := r.ListFailedIntegrationEvents(ctx, tenant, application.IntegrationOutboxFinancial, 10); err != nil || len(items) != 0 {
		t.Fatalf("after retry items=%+v err=%v", items, err)
	}
	if found, err := r.RetryFailedIntegrationEvent(ctx, tenant, application.IntegrationOutboxFinancial, rows[0].(*financialEffectiveOutboxRecord).ID, "ops-1"); err != nil || found {
		t.Fatalf("second retry found=%v err=%v", found, err)
	}

	// 四类队列都能被各自 dispatcher 再次领取。隔离库里可能残留其它测试租户的
	// pending 行，按 FIFO 继续认领直到命中本测试租户（每次认领会加锁），有界避免死循环。
	if id := claimUntilTenant(t, tenant, func() (string, string, bool, error) {
		delivery, found, err := r.ClaimFinancialEffectiveDelivery(ctx)
		return delivery.ID, delivery.TenantID, found, err
	}); id != rows[0].(*financialEffectiveOutboxRecord).ID {
		t.Fatalf("financial claimed id=%q want %q", id, rows[0].(*financialEffectiveOutboxRecord).ID)
	}
	if id := claimUntilTenant(t, tenant, func() (string, string, bool, error) {
		delivery, found, err := r.ClaimContractResponsibilityDelivery(ctx)
		return delivery.ID, delivery.TenantID, found, err
	}); id != rows[1].(*contractResponsibilityOutbox).ID {
		t.Fatalf("responsibility claimed id=%q want %q", id, rows[1].(*contractResponsibilityOutbox).ID)
	}
	if id := claimUntilTenant(t, tenant, func() (string, string, bool, error) {
		delivery, found, err := r.ClaimProjectDelivery(ctx)
		return delivery.ID, delivery.TenantID, found, err
	}); id != rows[2].(*projectDeliveryOutboxRecord).ID {
		t.Fatalf("project claimed id=%q want %q", id, rows[2].(*projectDeliveryOutboxRecord).ID)
	}
	if id := claimUntilTenant(t, tenant, func() (string, string, bool, error) {
		delivery, found, err := r.ClaimOpportunityLink(ctx)
		return delivery.ID, delivery.TenantID, found, err
	}); id != linkID {
		t.Fatalf("link claimed id=%q want %q", id, linkID)
	}
	// 每次重试必须在同一事务内写一条业务审计，不得伪造投递成功。
	var audits int64
	if err := r.db.Model(&auditOutboxRecord{}).
		Where("tenant_id = ? AND action = ?", tenant, "CONTRACT_MANAGEMENT:INTEGRATION_OUTBOX_RETRY").
		Count(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if audits != int64(len(application.IntegrationOutboxKinds())) {
		t.Fatalf("retry audit rows=%d", audits)
	}
}

// claimUntilTenant 有界认领队列，直到命中指定租户；隔离库存在其它测试租户时
// 仍能稳定验证“重试后可再次领取”，不依赖队列空表。
func claimUntilTenant(t *testing.T, tenant string, claim func() (id, tenantID string, found bool, err error)) string {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		id, tenantID, found, err := claim()
		if err != nil {
			t.Fatalf("claim attempt %d: %v", attempt, err)
		}
		if !found {
			t.Fatalf("claim attempt %d: queue is empty before %s row was claimable", attempt, tenant)
		}
		if tenantID == tenant {
			return id
		}
	}
	t.Fatalf("queue did not yield a %s row within 100 claims", tenant)
	return ""
}
