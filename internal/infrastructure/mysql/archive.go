package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) ArchiveExpired(ctx context.Context, in workflows.ExpiredArchiveInput) (workflows.ExpiredArchiveResult, error) {
	date, err := time.Parse(time.DateOnly, in.AsOfDate)
	if err != nil {
		return workflows.ExpiredArchiveResult{}, err
	}
	statuses := []contract.Status{contract.StatusActive, contract.StatusInProgress, contract.StatusPendingPay, contract.StatusCompleted, contract.StatusTerminated}
	var candidates []contractRecord
	err = r.db.WithContext(ctx).Select("id", "tenant_id", "owner_user_id", "status").
		Where("end_date < ? AND status IN ?", date, statuses).Order("end_date, id").Limit(1000).Find(&candidates).Error
	if err != nil {
		return workflows.ExpiredArchiveResult{}, err
	}
	result := workflows.ExpiredArchiveResult{}
	for _, candidate := range candidates {
		archived, err := r.archiveOne(ctx, candidate, in.AdditionalRecipients, date)
		if err != nil {
			return result, err
		}
		if archived {
			result.Archived++
		}
	}
	return result, nil
}

func (r *Repository) archiveOne(ctx context.Context, candidate contractRecord, additionalRecipients []string, date time.Time) (bool, error) {
	archived := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "tenant_id", "owner_user_id", "status", "end_date").
			Where("tenant_id = ? AND id = ?", candidate.TenantID, candidate.ID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		from := contract.Status(current.Status)
		if from != contract.Status(candidate.Status) || current.EndDate == nil || !current.EndDate.Before(date) || !contract.CanTransition(from, contract.StatusArchived) {
			return nil
		}
		var activeStatusChanges int64
		if err := tx.Model(&approvalInstanceRecord{}).
			Where("tenant_id = ? AND contract_id = ? AND kind = ? AND status = ?", current.TenantID, current.ID, "status_change", "running").
			Count(&activeStatusChanges).Error; err != nil {
			return err
		}
		if activeStatusChanges > 0 {
			return nil
		}
		if err := updateStatus(tx, current.TenantID, current.ID, from, contract.StatusArchived, "SYSTEM"); err != nil {
			return err
		}
		activityInput := workflows.StartApprovalActivityInput{TenantID: current.TenantID, ContractID: current.ID, WorkflowID: "contract-auto-archive-daily"}
		key := current.ID + ":auto-archive:" + date.Format(time.DateOnly)
		if err := insertLifecycle(tx, activityInput, from, contract.StatusArchived, "SYSTEM", "contract end date passed; automatically archived", key); err != nil {
			return err
		}
		now := time.Now().UTC()
		notifications := make([]notificationOutboxRecord, 0)
		for _, recipient := range uniqueStrings(append([]string{current.OwnerUserID}, additionalRecipients...)) {
			notifications = append(notifications, notificationOutboxRecord{ID: newID(), TenantID: current.TenantID, RecipientKey: "user:" + recipient, RecipientUserID: stringPtr(recipient), NotificationType: "status_change", Title: "合同已自动归档", Content: "合同结束日期已过，系统已自动归档", ContractID: stringPtr(current.ID), DedupeKey: key, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now})
		}
		for _, roleCode := range []string{"sales_director", "admin"} {
			notifications = append(notifications, notificationOutboxRecord{ID: newID(), TenantID: current.TenantID, RecipientKey: "role:" + roleCode, RecipientRoleCode: stringPtr(roleCode), NotificationType: "status_change", Title: "合同已自动归档", Content: "合同结束日期已过，系统已自动归档", ContractID: stringPtr(current.ID), DedupeKey: key, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now})
		}
		if len(notifications) > 0 {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&notifications).Error; err != nil {
				return err
			}
		}
		archived = true
		return nil
	})
	return archived, err
}
