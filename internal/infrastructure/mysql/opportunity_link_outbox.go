package mysql

import (
	"context"
	"encoding/json"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	crmintegration "github.com/j-s-te/contract-management/internal/integration/crm"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// enqueueOpportunityLink persists the callback payload in the same transaction
// as the review, so a confirmed intake can never lose its CRM notification.
func enqueueOpportunityLink(tx *gorm.DB, item application.OpportunityIntake, payload []byte) error {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&opportunityLinkOutboxRecord{
		ID: ulid.Make().String(), TenantID: item.TenantID, IntakeID: item.IntakeID,
		EventID: item.EventID, PayloadJSON: payload, DeliveryStatus: "pending",
		NextAttemptAt: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}).Error
}

func (r *Repository) ClaimOpportunityLink(ctx context.Context) (crmintegration.LinkDelivery, bool, error) {
	var delivery crmintegration.LinkDelivery
	found := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row opportunityLinkOutboxRecord
		now := time.Now().UTC()
		result := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where(
			"(delivery_status = ? AND next_attempt_at <= ?) OR (delivery_status = ? AND locked_at < ?)",
			"pending", now, "processing", now.Add(-5*time.Minute),
		).Order("next_attempt_at ASC, created_at ASC").Limit(1).Find(&row)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		attempts := row.Attempts + 1
		if err := tx.Model(&opportunityLinkOutboxRecord{}).Where("id = ?", row.ID).Updates(map[string]any{
			"delivery_status": "processing", "attempts": attempts, "locked_at": now,
		}).Error; err != nil {
			return err
		}
		delivery = crmintegration.LinkDelivery{ID: row.ID, TenantID: row.TenantID, Payload: row.PayloadJSON, Attempts: attempts}
		found = true
		return nil
	})
	return delivery, found, err
}

func (r *Repository) MarkOpportunityLinkDelivered(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&opportunityLinkOutboxRecord{}).Where("id = ? AND delivery_status = ?", id, "processing").Updates(map[string]any{
		"delivery_status": "delivered", "delivered_at": now, "locked_at": nil, "last_error": nil,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrNotFound
	}
	return nil
}

func (r *Repository) MarkOpportunityLinkFailed(ctx context.Context, id, message string, attempts uint, dead bool) error {
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
	return r.db.WithContext(ctx).Model(&opportunityLinkOutboxRecord{}).Where("id = ? AND delivery_status = ?", id, "processing").Updates(map[string]any{
		"delivery_status": status, "next_attempt_at": time.Now().UTC().Add(delay), "locked_at": nil, "last_error": message,
	}).Error
}

// DecodeOpportunityLinkPayload 解码持久化队列中的稳定 CRM 回调协议，供测试和运维诊断使用。
func DecodeOpportunityLinkPayload(payload []byte) (crmintegration.OpportunityLinkCallback, error) {
	var item crmintegration.OpportunityLinkCallback
	if err := json.Unmarshal(payload, &item); err != nil {
		return item, err
	}
	return item, nil
}
