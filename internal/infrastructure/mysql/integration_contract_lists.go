package mysql

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

// integrationConsumableScope 在原查询排序和LIMIT之前排除本地不适用来源，兼容旧NULL。
// 参数：query为已有可信租户及责任范围的合同查询。
// 返回值：仅供项目/结算集成使用的查询，不影响普通合同台账。
func integrationConsumableScope(query *gorm.DB) *gorm.DB {
	return query.Where("(integration_policy IS NULL OR integration_policy <> ?)", contract.IntegrationNotApplicable).
		Where("COALESCE(counterparty_source,'') <> 'LOCAL' AND COALESCE(category_source,'') <> 'LOCAL'")
}

// ListIntegrationApprovedContractsScoped 返回项目可消费的已审批合同，先裁剪来源再应用有界窗口。
// 参数：ctx/filter为可信租户及原责任范围，limit为数量上限。
// 返回值：合同及付款期，或存储错误；旧CRM的NULL策略不被隐藏。
func (r *Repository) ListIntegrationApprovedContractsScoped(ctx context.Context, filter contract.ScopeFilter, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "").Where("status IN ?", contract.ApprovalPassedStatuses())
	var rows []contractRecord
	if err := integrationConsumableScope(query).Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]contract.Contract, 0, len(rows))
	for _, row := range rows {
		values = append(values, contractFromRecord(row))
	}
	return r.attachPaymentTerms(ctx, filter.TenantID, values)
}

// ListIntegrationCompletedContractsScoped 返回结算可消费的completed合同，来源过滤在LIMIT之前。
// 参数：ctx/filter为可信范围，keyword为原列表检索，limit为数量上限。
// 返回值：合同及付款计划，或存储错误；不改变普通台账。
func (r *Repository) ListIntegrationCompletedContractsScoped(ctx context.Context, filter contract.ScopeFilter, keyword string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "").Where("status = ?", contract.StatusCompleted)
	if keyword != "" {
		// 与普通台账共用 literalLikePattern：结算消费列表的关键字口径不能更宽，
		// 否则同一关键字在两个列表里命中不同行（% _ 未转义时结算侧会多出记录）。
		pattern := literalLikePattern(keyword)
		query = query.Where("(contract_number LIKE ? OR title LIKE ? OR customer_name LIKE ?)", pattern, pattern, pattern)
	}
	var rows []contractRecord
	if err := integrationConsumableScope(query).Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]contract.Contract, 0, len(rows))
	for _, row := range rows {
		values = append(values, contractFromRecord(row))
	}
	return r.attachPaymentTerms(ctx, filter.TenantID, values)
}
