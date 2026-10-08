package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) UpdateContractDraft(ctx context.Context, c contract.Contract, expectedVersion uint64, actorUserID string) error {
	values, err := json.Marshal(c.TemplateValues)
	if err != nil {
		return err
	}
	systems, err := json.Marshal(c.Systems)
	if err != nil {
		return err
	}
	items, err := json.Marshal(c.ServiceItems)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, c.ID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return apperrors.ErrVersionConflict
		}
		if contract.Status(current.Status) != contract.StatusDraft {
			return apperrors.ErrStateConflict
		}
		now := time.Now().UTC()
		// Only editable draft content is updated. Identity, owner, source binding,
		// contract number and original creation audit cannot be changed by callers.
		result := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ? AND version = ? AND status = ?", c.TenantID, c.ID, expectedVersion, contract.StatusDraft).Updates(map[string]any{
			"title": c.Title, "contract_type": c.Type, "service_type": c.ServiceType, "customer_credit_level": stringPtr(c.CustomerCreditLevel),
			"opportunity_id": stringPtr(c.OpportunityID), "opportunity_name": stringPtr(c.OpportunityName), "crm_customer_id": uintPtr(c.CRMCustomerID),
			"customer_name": stringPtr(c.CustomerName), "customer_address": stringPtr(c.CustomerAddress), "customer_contact": stringPtr(c.CustomerContact), "customer_phone": stringPtr(c.CustomerPhone),
			"systems_json": systems, "service_items_json": items, "amount_minor": c.AmountMinor, "currency": c.Currency,
			"content": c.Content, "template_values_json": values, "rendered_document": c.Document, "content_hash": c.ContentHash,
			"start_date": c.StartDate, "end_date": c.EndDate, "version": expectedVersion + 1, "updated_at": now, "updated_by": actorUserID,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrVersionConflict
		}
		return tx.Create(&lifecycleEventRecord{ID: newID(), TenantID: c.TenantID, ContractID: c.ID, FromStatus: string(contract.StatusDraft), ToStatus: string(contract.StatusDraft), ActorUserID: stringPtr(actorUserID), Reason: stringPtr("contract draft edited"), IdempotencyKey: fmt.Sprintf("%s:draft:v%d", c.ID, expectedVersion+1), OccurredAt: now}).Error
	})
}
