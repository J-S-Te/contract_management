package mysql

import (
	"context"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// CountSignedContractsByOpportunityIDs 批量统计各商机的普通已签约合同，不把历史导入当赢单证据。
// 参数：ctx为上下文，tenantID为可信租户，opportunityIDs为商机标识集合。
// 返回值：按商机分组的数量或数据库错误。
func (r *Repository) CountSignedContractsByOpportunityIDs(ctx context.Context, tenantID string, opportunityIDs []string) (map[string]uint64, error) {
	type countRow struct {
		OpportunityID string `gorm:"column:opportunity_id"`
		Count         uint64 `gorm:"column:signed_contract_count"`
	}
	rows := make([]countRow, 0, len(opportunityIDs))
	err := ordinaryContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), "").
		Select("opportunity_id, COUNT(*) AS signed_contract_count").
		Where("tenant_id = ? AND opportunity_id IN ? AND status IN ?", tenantID, opportunityIDs, contract.ApprovalPassedStatuses()).
		Group("opportunity_id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make(map[string]uint64, len(opportunityIDs))
	for _, opportunityID := range opportunityIDs {
		result[opportunityID] = 0
	}
	for _, row := range rows {
		result[row.OpportunityID] = row.Count
	}
	return result, nil
}
