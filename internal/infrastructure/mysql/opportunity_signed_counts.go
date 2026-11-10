package mysql

import (
	"context"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// CountSignedContractsByOpportunityIDs 统计商机关联的普通及已确认历史已签合同，仅用于展示。
// 参数：ctx为上下文，tenantID为可信租户，opportunityIDs为商机标识集合。
// 返回值：按商机分组的累计数量或数据库错误；本统计不提供普通赢单证据。
func (r *Repository) CountSignedContractsByOpportunityIDs(ctx context.Context, tenantID string, opportunityIDs []string) (map[string]uint64, error) {
	type countRow struct {
		OpportunityID string `gorm:"column:opportunity_id"`
		Count         uint64 `gorm:"column:signed_contract_count"`
	}
	rows := make([]countRow, 0, len(opportunityIDs))
	// 历史合同的签署声明须已走完确认；不能因仅有草稿或归档状态就计入已签数量。
	// 复用原普通来源条件，但不修改普通合同引用、审批及财务派生的来源隔离。
	sourceScope := ordinaryContractScope(r.db.WithContext(ctx), "").Or(
		"origin_kind = ? AND import_mode IN ? "+
			"AND import_confirmed_at IS NOT NULL AND declared_signed_date IS NOT NULL "+
			"AND import_policy_json IS NOT NULL",
		"IMPORTED", []contract.ImportMode{
			contract.ImportArchiveOnly, contract.ImportHistoricalClosed, contract.ImportContinueLive,
		})
	err := r.db.WithContext(ctx).Model(&contractRecord{}).Where(sourceScope).
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
