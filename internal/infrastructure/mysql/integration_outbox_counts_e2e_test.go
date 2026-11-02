package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/oklog/ulid/v2"
)

// BIZ-XEVT2-03：/readyz 死信计数必须来自四张真实 Outbox 表的 dead 行，
// 而不是内存或伪造值。参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN
// 指向隔离库时运行。返回值：无。
func TestCountDeadIntegrationOutboxEventsOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	contractID := seedHandoverContract(t, r.db, tenant, ulid.Make().String(), "active")
	intakeID := ulid.Make().String()
	now := time.Now().UTC()
	intake := opportunityIntakeRecord{
		IntakeID: intakeID, TenantID: tenant, EventID: ulid.Make().String(), OpportunityID: 9,
		EventVersion: 1, OpportunityNo: "SJ-COUNT", CustomerID: 43, ContractRef: contractID,
		ContractID: &contractID, ExpectedAmount: "200.00", OccurredAt: now,
		Status: application.OpportunityIntakeLinkConfirmed, Version: 2, CreatedAt: now, UpdatedAt: now,
	}
	if err := r.db.Create(&intake).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 死信与接入记录不在通用隔离清理列表内，按依赖顺序显式清理本测试租户。
		for _, model := range []any{&opportunityLinkOutboxRecord{}, &opportunityIntakeReviewRecord{}, &opportunityIntakeRecord{}, &auditOutboxRecord{}} {
			if err := r.db.Where("tenant_id = ?", tenant).Delete(model).Error; err != nil {
				t.Error("integration outbox count test cleanup failed:", err)
			}
		}
	})
	rows := []any{
		&financialEffectiveOutboxRecord{ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: 11, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
		&contractResponsibilityOutbox{ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: 12, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
		&projectDeliveryOutboxRecord{ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: 13, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
		&opportunityLinkOutboxRecord{ID: ulid.Make().String(), TenantID: tenant, IntakeID: intakeID, EventID: intake.EventID, PayloadJSON: []byte(`{}`), DeliveryStatus: "dead", Attempts: 20, NextAttemptAt: now, CreatedAt: now},
	}
	for _, row := range rows {
		if err := r.db.Create(row).Error; err != nil {
			t.Fatalf("seed dead outbox row: %v", err)
		}
	}

	counts, err := r.CountDeadIntegrationOutboxEvents(ctx)
	if err != nil {
		t.Fatalf("count dead outbox events: %v", err)
	}
	if len(counts) != len(application.IntegrationOutboxKinds()) {
		t.Fatalf("counts = %+v, want %d kinds", counts, len(application.IntegrationOutboxKinds()))
	}
	// 计数是部署级聚合，隔离库中其他用例可能已有 dead 行，因此断言 >= 1。
	for _, kind := range application.IntegrationOutboxKinds() {
		if counts[kind] < 1 {
			t.Fatalf("%s dead = %d, want >= 1; counts = %+v", kind, counts[kind], counts)
		}
	}
}
