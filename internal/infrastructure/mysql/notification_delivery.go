package mysql

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/j-s-te/contract-management/internal/apperrors"
	notificationintegration "github.com/j-s-te/contract-management/internal/integration/notification"
)

// ClaimNotificationDelivery 领取一条待投递的通知并写入有限租约，超时的 processing 记录可被回收。
func (r *Repository) ClaimNotificationDelivery(ctx context.Context) (notificationintegration.Delivery, bool, error) {
	var delivery notificationintegration.Delivery
	found := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row notificationOutboxRecord
		now := time.Now().UTC()
		err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("(delivery_status = ? AND next_attempt_at <= ?) OR (delivery_status = ? AND locked_at < ?)", "pending", now, "processing", now.Add(-5*time.Minute)).
			Order("next_attempt_at ASC, created_at ASC").Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		attempts := row.Attempts + 1
		if err := tx.Model(&notificationOutboxRecord{}).Where("id = ?", row.ID).
			Updates(map[string]any{"delivery_status": "processing", "attempts": attempts, "locked_at": now}).Error; err != nil {
			return err
		}
		delivery = notificationintegration.Delivery{
			ID: row.ID, TenantID: row.TenantID, RecipientKey: row.RecipientKey,
			RecipientUserID: row.RecipientUserID, RecipientRoleCode: row.RecipientRoleCode,
			NotificationType: row.NotificationType, Title: row.Title, Content: row.Content,
			ContractID: row.ContractID, ApprovalID: row.ApprovalID, DedupeKey: row.DedupeKey,
			Attempts: attempts, CreatedAt: row.CreatedAt,
		}
		found = true
		return nil
	})
	return delivery, found, err
}

func (r *Repository) MarkNotificationDelivered(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&notificationOutboxRecord{}).Where("id = ? AND delivery_status = ?", id, "processing").
		Updates(map[string]any{"delivery_status": "delivered", "delivered_at": now, "locked_at": nil, "last_error": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *Repository) MarkNotificationFailed(ctx context.Context, id, message string, attempts uint, dead bool) error {
	status := "pending"
	if dead {
		status = "dead"
	}
	delay := time.Second
	for count := uint(1); count < attempts && delay < 10*time.Minute; count++ {
		delay *= 2
	}
	if delay > 10*time.Minute {
		delay = 10 * time.Minute
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	return r.db.WithContext(ctx).Model(&notificationOutboxRecord{}).Where("id = ? AND delivery_status = ?", id, "processing").
		Updates(map[string]any{"delivery_status": status, "next_attempt_at": time.Now().UTC().Add(delay), "locked_at": nil, "last_error": message}).Error
}
