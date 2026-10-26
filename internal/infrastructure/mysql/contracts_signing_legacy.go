package mysql

import (
	"context"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func (r *Repository) ListSigningRecords(ctx context.Context, tenantID string, limit int) ([]contract.SigningRecord, error) {
	contracts, err := r.ListApprovedContracts(ctx, tenantID, limit)
	if err != nil || len(contracts) == 0 {
		return nil, err
	}
	ids := make([]string, 0, len(contracts))
	for _, item := range contracts {
		ids = append(ids, item.ID)
	}
	var signingRows []signingRecord
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Find(&signingRows).Error; err != nil {
		return nil, err
	}
	var documents []stampedDocumentRecord
	if err := r.db.WithContext(ctx).Select("contract_id", "version", "original_filename", "content_sha256", "uploaded_at").
		Where("tenant_id = ? AND contract_id IN ?", tenantID, ids).Order("contract_id ASC, version ASC").Find(&documents).Error; err != nil {
		return nil, err
	}
	byID := make(map[string]signingRecord, len(signingRows))
	for _, row := range signingRows {
		byID[row.ContractID] = row
	}
	docByID := make(map[string]stampedDocumentRecord, len(documents))
	for _, row := range documents {
		docByID[row.ContractID] = row
	}
	result := make([]contract.SigningRecord, 0, len(contracts))
	for _, item := range contracts {
		result = append(result, signingFromRecords(item, byID[item.ID], docByID[item.ID]))
	}
	return result, nil
}
