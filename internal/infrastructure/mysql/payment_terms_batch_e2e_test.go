package mysql

import (
	"context"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// claimedBatchRow 是批量认领的可观测投影（id/租户/尝试次数）。
type claimedBatchRow struct {
	ID       string
	TenantID string
	Attempts uint
}

// claimFinancialBatch 调用仓储批量认领财务事件并投影结果。
// 参数：ctx 控制认领事务，r 为仓储，limit 为本轮上限。
// 返回值：投影行及存储错误。
func claimFinancialBatch(ctx context.Context, r *Repository, limit int) ([]claimedBatchRow, error) {
	deliveries, err := r.ClaimFinancialEffectiveDeliveries(ctx, limit)
	rows := make([]claimedBatchRow, 0, len(deliveries))
	for _, delivery := range deliveries {
		rows = append(rows, claimedBatchRow{ID: delivery.ID, TenantID: delivery.TenantID, Attempts: delivery.Attempts})
	}
	return rows, err
}

// claimResponsibilityBatch 调用仓储批量认领责任事件并投影结果。
// 参数：ctx 控制认领事务，r 为仓储，limit 为本轮上限。
// 返回值：投影行及存储错误。
func claimResponsibilityBatch(ctx context.Context, r *Repository, limit int) ([]claimedBatchRow, error) {
	deliveries, err := r.ClaimContractResponsibilityDeliveries(ctx, limit)
	rows := make([]claimedBatchRow, 0, len(deliveries))
	for _, delivery := range deliveries {
		rows = append(rows, claimedBatchRow{ID: delivery.ID, TenantID: delivery.TenantID, Attempts: delivery.Attempts})
	}
	return rows, err
}

// TestBatchDeliveryClaimOnIsolatedMySQL 验证财务/责任队列的批量认领语义：
// 单轮最多领取 limit 行、每行仍各自写 attempts+1 与 processing 租约、
// 已认领行在租约内不会被重复领取。顺序不参与投递语义。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestBatchDeliveryClaimOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	contractID := seedHandoverContract(t, r.db, tenant, ulid.Make().String(), "active")
	now := time.Now().UTC()
	financialIDs := make(map[string]struct{}, 3)
	responsibilityIDs := make(map[string]struct{}, 3)
	for index := 0; index < 3; index++ {
		financial := financialEffectiveOutboxRecord{
			ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: uint64(index + 1),
			PayloadJSON:    []byte(`{"event_type":"contract.financial_effective.v1"}`),
			DeliveryStatus: "pending", NextAttemptAt: now.Add(-time.Minute), CreatedAt: now.Add(time.Duration(index) * time.Second),
		}
		responsibility := contractResponsibilityOutbox{
			ID: ulid.Make().String(), TenantID: tenant, ContractID: contractID, ContractVersion: uint64(index + 1),
			PayloadJSON:    []byte(`{"event_type":"contract.responsibility_changed.v1"}`),
			DeliveryStatus: "pending", NextAttemptAt: now.Add(-time.Minute), CreatedAt: now.Add(time.Duration(index) * time.Second),
		}
		if err := r.db.Create(&financial).Error; err != nil {
			t.Fatal(err)
		}
		if err := r.db.Create(&responsibility).Error; err != nil {
			t.Fatal(err)
		}
		financialIDs[financial.ID] = struct{}{}
		responsibilityIDs[responsibility.ID] = struct{}{}
	}

	claimedFinancial := claimBatchUntilTenant(t, tenant, financialIDs, func(limit int) ([]claimedBatchRow, error) {
		return claimFinancialBatch(ctx, r, limit)
	})
	if len(claimedFinancial) != len(financialIDs) {
		t.Fatalf("财务批量认领覆盖 %d 行，want %d", len(claimedFinancial), len(financialIDs))
	}
	for id, attempts := range claimedFinancial {
		if attempts != 1 {
			t.Fatalf("财务行 %s attempts=%d，批量认领必须逐行记一次尝试", id, attempts)
		}
	}
	claimedResponsibility := claimBatchUntilTenant(t, tenant, responsibilityIDs, func(limit int) ([]claimedBatchRow, error) {
		return claimResponsibilityBatch(ctx, r, limit)
	})
	if len(claimedResponsibility) != len(responsibilityIDs) {
		t.Fatalf("责任批量认领覆盖 %d 行，want %d", len(claimedResponsibility), len(responsibilityIDs))
	}

	// 认领后必须处于 processing + 租约，且 attempts=1；不能伪造 delivered。
	for id := range financialIDs {
		var row financialEffectiveOutboxRecord
		if err := r.db.Where("id = ?", id).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.DeliveryStatus != "processing" || row.Attempts != 1 || row.LockedAt == nil || row.DeliveredAt != nil {
			t.Fatalf("财务行状态=%+v", row)
		}
	}
	// 已认领行再次认领必须为空（租约未过期），证明批量认领不会重复投递同一条。
	reclaimed, err := claimFinancialBatch(ctx, r, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range reclaimed {
		if _, ours := financialIDs[row.ID]; ours {
			t.Fatalf("已认领行被重复领取：%s", row.ID)
		}
	}
}

// claimBatchUntilTenant 有界批量认领，直到本测试租户的全部目标行都被认领；
// 隔离库可能残留其它租户的 pending 行，因此按批推进并限制轮数。
// 参数：t 为测试上下文，tenant 为测试租户，want 为目标行集合，claim 为批量认领回调。
// 返回值：目标行→认领 attempts 的映射。
func claimBatchUntilTenant(t *testing.T, tenant string, want map[string]struct{}, claim func(int) ([]claimedBatchRow, error)) map[string]uint {
	t.Helper()
	found := make(map[string]uint, len(want))
	for round := 0; round < 100 && len(found) < len(want); round++ {
		rows, err := claim(2)
		if err != nil {
			t.Fatalf("batch claim round %d: %v", round, err)
		}
		if len(rows) > 2 {
			t.Fatalf("批量认领返回 %d 行，超过请求上限 2", len(rows))
		}
		if len(rows) == 0 {
			t.Fatalf("batch claim round %d: 队列提前为空", round)
		}
		for _, row := range rows {
			if row.TenantID != tenant {
				continue
			}
			if _, ok := want[row.ID]; ok {
				found[row.ID] = row.Attempts
			}
		}
	}
	return found
}
