package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

// DiscountMetrics 在同一只读RR事务读取完整NORMAL合同与冻结定价。
// 参数：ctx为请求，tenant为已认证机器租户。
// 返回值：完整事实；超行数、字节上限或事务失败时拒绝整个快照。
func (r *Repository) DiscountMetrics(ctx context.Context, tenant string) (application.DiscountMetrics, error) {
	out := application.DiscountMetrics{SchemaVersion: 1, TenantID: tenant, SourceCode: "contract_management", Basis: "原生NORMAL合同生效日归属；同币种同税基冻结标准价，历史无基准列未知", Rows: []application.DiscountMetricRow{}}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []contractRecord
		if err := ordinaryContractScope(tx.Model(&contractRecord{}), "").Where("tenant_id=?", tenant).Order("id ASC").Limit(10001).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > 10000 {
			return fmt.Errorf("dashboard metrics exceeds 10000 rows")
		}
		var updated, started time.Time
		for _, row := range rows {
			if row.AmountMinor < 0 || row.AmountMinor > contract.MaxPricingMinor {
				return fmt.Errorf("contract amount exceeds safe metrics bound")
			}
			c := contractFromRecord(row)
			out.Rows = append(out.Rows, application.DiscountRow(c))
			if row.UpdatedAt.After(updated) {
				updated = row.UpdatedAt
			}
			if c.Pricing != nil && !c.Pricing.FrozenAt.IsZero() && (started.IsZero() || c.Pricing.FrozenAt.Before(started)) {
				started = c.Pricing.FrozenAt
			}
		}
		out.SnapshotAt = time.Now().UTC().Format(time.RFC3339Nano)
		if !updated.IsZero() {
			out.SourceUpdatedAt = updated.UTC().Format(time.RFC3339Nano)
		}
		if !started.IsZero() {
			out.CollectionStartedAt = started.UTC().Format(time.RFC3339Nano)
		}
		out.Complete = true
		data, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if len(data) > 8*1024*1024-4096 {
			return fmt.Errorf("dashboard metrics exceeds 8MiB")
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return out, err
}
