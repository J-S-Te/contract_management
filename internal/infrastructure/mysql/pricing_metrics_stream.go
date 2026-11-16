package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/metricstream"
	"gorm.io/gorm"
	"io"
	"time"
)

// StreamDiscountMetrics 按合同主键游标在同一RR事务中输出全部历史指标。
// 参数：ctx 为取消上下文，tenant 为验签租户，output 为有界协议输出。
// 返回值：读取、边界或提交错误；失败时永不输出end。
func (r *Repository) StreamDiscountMetrics(ctx context.Context, tenant string, output io.Writer) error {
	if tenant == "" {
		return application.ErrForbidden
	}
	meta := application.DiscountMetrics{SchemaVersion: 1, TenantID: tenant, SourceCode: "contract_management", SnapshotAt: time.Now().UTC().Format(time.RFC3339Nano), Complete: true, Basis: "原生NORMAL合同生效日归属；同币种同税基冻结标准价，历史无基准列未知", Rows: []application.DiscountMetricRow{}}
	var stream *metricstream.Writer
	var updated, started time.Time
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		stream, err = metricstream.New(output, meta)
		if err != nil {
			return err
		}
		cursor := ""
		for {
			var rows []contractRecord
			query := ordinaryContractScope(tx.Model(&contractRecord{}), "").Select(discountMetricColumns).Where("tenant_id=? AND id>?", tenant, cursor)
			if err := query.Order("id").Limit(metricstream.MaxRows).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				return stream.Flush()
			}
			for _, row := range rows {
				if row.AmountMinor < 0 || row.AmountMinor > contract.MaxPricingMinor {
					return fmt.Errorf("contract amount exceeds safe metrics bound")
				}
				c := contractFromRecord(row)
				if err := stream.Add(application.DiscountRow(c)); err != nil {
					return err
				}
				if row.UpdatedAt.After(updated) {
					updated = row.UpdatedAt
				}
				if c.Pricing != nil && !c.Pricing.FrozenAt.IsZero() && (started.IsZero() || c.Pricing.FrozenAt.Before(started)) {
					started = c.Pricing.FrozenAt
				}
				cursor = row.ID
			}
		}
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return stream.End(metricStreamTime(updated), metricStreamTime(started))
}

// metricStreamTime 将存在的源事实时间格式化为协议时间。
// 参数：at 为源事实时间，零值表示未采集。
// 返回值：UTC时间或空串。
func metricStreamTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}
