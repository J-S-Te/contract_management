package mysql

import (
	"context"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ListApprovedContracts 读取租户内普通已批准合同的有界列表。
// 参数：ctx为上下文，tenantID为可信租户，limit为数量上限。
// 返回值：合同列表及付款计划，或数据库错误。
func (r *Repository) ListApprovedContracts(ctx context.Context, tenantID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := ordinaryContractScope(r.db.WithContext(ctx), "").Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses()).
		Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, tenantID, result)
}

// ListApprovedContractReferences 按游标读取普通已批准合同引用，避免载入原件。
// 参数：ctx为上下文，tenantID为租户，afterID为游标，limit为页大小。
// 返回值：可见引用列表或数据库错误。
func (r *Repository) ListApprovedContractReferences(ctx context.Context, tenantID, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := ordinaryContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), "").
		Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses())
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	var records []contractRecord
	if err := query.Select("id", "contract_number", "version").Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contract.Contract{ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Version: record.Version})
	}
	return result, nil
}

// ListApprovedContractsScoped 按可信责任范围读取普通已批准合同及付款计划。
// 参数：ctx为上下文，filter为租户及责任范围，limit为数量上限。
// 返回值：范围内合同列表或数据库错误。
func (r *Repository) ListApprovedContractsScoped(ctx context.Context, filter contract.ScopeFilter, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "").
		Where("status IN ?", contract.ApprovalPassedStatuses()).Omit("rendered_document", "template_values_json").
		Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, filter.TenantID, result)
}

// ListApprovedContractReferencesScoped 按责任范围和游标读取普通已批准合同引用。
// 参数：ctx为上下文，filter为可信范围，afterID为游标，limit为页大小。
// 返回值：可见合同引用或数据库错误。
func (r *Repository) ListApprovedContractReferencesScoped(ctx context.Context, filter contract.ScopeFilter, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "").
		Where("status IN ?", contract.ApprovalPassedStatuses())
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	var records []contractRecord
	if err := query.Select("id", "contract_number", "version").Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contract.Contract{ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Version: record.Version})
	}
	return result, nil
}
