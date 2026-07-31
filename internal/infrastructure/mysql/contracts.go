package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) TransitionDirect(ctx context.Context, tenantID, contractID string, expectedVersion uint64, target contract.Status, actorUserID, reason, idempotencyKey string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "tenant_id", "status", "version").
			Where("tenant_id = ? AND id = ?", tenantID, contractID).
			Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if row.Version != expectedVersion {
			return apperrors.ErrVersionConflict
		}
		var activeStatusChanges int64
		if err := tx.Model(&approvalInstanceRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND kind = ? AND status = ?", tenantID, contractID, "status_change", "running").
			Count(&activeStatusChanges).Error; err != nil {
			return err
		}
		if activeStatusChanges > 0 {
			return apperrors.ErrStateConflict
		}
		if target.RequiresApproval() || target == contract.StatusPending || target == contract.StatusApproved || target == contract.StatusActive {
			return apperrors.ErrStateConflict
		}
		from := contract.Status(row.Status)
		if err := updateStatus(tx, tenantID, contractID, from, target, actorUserID); err != nil {
			return err
		}
		in := workflows.StartApprovalActivityInput{TenantID: tenantID, ContractID: contractID}
		return insertLifecycle(tx, in, from, target, actorUserID, reason, idempotencyKey)
	})
}

func (r *Repository) CreateContract(ctx context.Context, c contract.Contract, actorUserID string) error {
	now := time.Now().UTC()
	if c.Status == "" {
		c.Status = contract.StatusDraft
	}
	record := contractRecord{
		ID: c.ID, TenantID: c.TenantID, ContractNumber: c.Number, Title: c.Title,
		ContractType: c.Type, ServiceType: c.ServiceType, CustomerCreditLevel: stringPtr(c.CustomerCreditLevel),
		OwnerUserID: c.OwnerUserID, AmountMinor: c.AmountMinor, Currency: c.Currency, Content: c.Content,
		Status: string(c.Status), EndDate: c.EndDate, ContentHash: stringPtr(c.ContentHash), Version: 1,
		CreatedAt: now, CreatedBy: actorUserID, UpdatedAt: now, UpdatedBy: actorUserID,
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return tx.Create(&lifecycleEventRecord{
			ID: newID(), TenantID: c.TenantID, ContractID: c.ID,
			FromStatus: string(contract.StatusDraft), ToStatus: string(contract.StatusDraft),
			ActorUserID: stringPtr(actorUserID), Reason: stringPtr("contract created"),
			IdempotencyKey: c.ID + ":created", OccurredAt: now,
		}).Error
	})
}
