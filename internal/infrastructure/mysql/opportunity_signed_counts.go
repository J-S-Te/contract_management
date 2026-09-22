package mysql

import (
	"context"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func (r *Repository) CountSignedContractsByOpportunityIDs(ctx context.Context, tenantID string, opportunityIDs []string) (map[string]uint64, error) {
	type countRow struct {
		OpportunityID string `gorm:"column:opportunity_id"`
		Count         uint64 `gorm:"column:signed_contract_count"`
	}
	rows := make([]countRow, 0, len(opportunityIDs))
	err := r.db.WithContext(ctx).Model(&contractRecord{}).
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
