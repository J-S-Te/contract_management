package mysql

import (
	"context"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func (r *Repository) ListApprovedContracts(ctx context.Context, tenantID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND status IN ?", tenantID, contract.ApprovalPassedStatuses()).
		Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, tenantID, result)
}

func (r *Repository) ListApprovedContractReferences(ctx context.Context, tenantID, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := r.db.WithContext(ctx).Model(&contractRecord{}).
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

func (r *Repository) ListApprovedContractsScoped(ctx context.Context, filter contract.ScopeFilter, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []contractRecord
	if err := applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter).
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

func (r *Repository) ListApprovedContractReferencesScoped(ctx context.Context, filter contract.ScopeFilter, afterID string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	query := applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter).
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
