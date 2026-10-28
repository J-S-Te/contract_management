package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	settlementintegration "github.com/j-s-te/contract-management/internal/integration/settlement"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type contractResponsibilityStream struct {
	TenantID    string `gorm:"primaryKey"`
	ContractID  string `gorm:"primaryKey"`
	OwnerUserID string
	Active      bool
	Version     uint64
	UpdatedAt   time.Time
}

// TableName 指定独立责任版本表。
// 参数：无。
// 返回值：责任版本表名。
func (contractResponsibilityStream) TableName() string { return "con_contract_responsibility_stream" }

type contractResponsibilityOutbox financialEffectiveOutboxRecord

// TableName 指定独立责任投递表。
// 参数：无。
// 返回值：责任Outbox表名。
func (contractResponsibilityOutbox) TableName() string { return "con_contract_responsibility_outbox" }

// responsibilityActive 判断合同是否保留当前负责人的业务责任。
// 参数：row 为权威合同行。
// 返回值：负责人存在且合同未撤销或归档时为 true。
func responsibilityActive(row contractRecord) bool {
	return row.OwnerUserID != "" && row.Status != "terminated" && row.Status != "archived"
}

// newResponsibilityPayload 构造不含金额的独立责任事件。
// 参数：eventID 为稳定事件号，row 为权威合同，version 为责任版本，now 为发生时间。
// 返回值：冻结JSON载荷及序列化错误。
func newResponsibilityPayload(eventID string, row contractRecord, version uint64, now time.Time) ([]byte, error) {
	return json.Marshal(struct {
		EventID          string    `json:"event_id"`
		EventType        string    `json:"event_type"`
		TenantID         string    `json:"tenant_id"`
		OccurredAt       time.Time `json:"occurred_at"`
		SourceContractID string    `json:"source_contract_id"`
		SourceVersion    uint64    `json:"source_version"`
		OwnerUserID      string    `json:"owner_user_id"`
		Active           bool      `json:"active"`
	}{eventID, "contract.responsibility_changed.v1", row.TenantID, now.UTC(), row.ID, version, row.OwnerUserID, responsibilityActive(row)})
}

// enqueueContractResponsibility 在合同行锁事务内推进独立责任版本并写Outbox。
// 参数：tx 为当前业务事务，tenantID 和 contractID 限定权威合同。
// 返回值：任一存储失败都会返回错误并由调用方回滚整个业务事务。
func enqueueContractResponsibility(tx *gorm.DB, tenantID, contractID string) error {
	var row contractRecord
	if err := tx.Where("tenant_id=? AND id=?", tenantID, contractID).Take(&row).Error; err != nil {
		return err
	}
	seed := contractResponsibilityStream{TenantID: tenantID, ContractID: contractID, UpdatedAt: time.Now().UTC()}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
		return err
	}
	var stream contractResponsibilityStream
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND contract_id=?", tenantID, contractID).Take(&stream).Error; err != nil {
		return err
	}
	active := responsibilityActive(row)
	if stream.Version > 0 && stream.OwnerUserID == row.OwnerUserID && stream.Active == active {
		return nil
	}
	stream.Version++
	stream.OwnerUserID = row.OwnerUserID
	stream.Active = active
	stream.UpdatedAt = time.Now().UTC()
	if err := tx.Save(&stream).Error; err != nil {
		return err
	}
	record := contractResponsibilityOutbox{
		ID: newID(), TenantID: tenantID, ContractID: contractID, ContractVersion: stream.Version,
		DeliveryStatus: "pending", NextAttemptAt: stream.UpdatedAt, CreatedAt: stream.UpdatedAt}
	payload, err := newResponsibilityPayload(record.ID, row, stream.Version, stream.UpdatedAt)
	if err != nil {
		return err
	}
	record.PayloadJSON = payload
	return tx.Create(&record).Error
}

// ReconcileContractResponsibilities 为旧合同补建缺失的真实责任基线。
// 参数：ctx 控制分批扫描和逐合同事务。
// 返回值：已处理数量及错误；不会猜测缺失负责人。
func (r *Repository) ReconcileContractResponsibilities(ctx context.Context) (int, error) {
	count := 0
	for {
		var rows []projectDeliveryCandidate
		if err := r.db.WithContext(ctx).Table("con_contract AS c").Select("c.tenant_id,c.id AS contract_id").Where("NOT EXISTS (SELECT 1 FROM con_contract_responsibility_stream s WHERE s.tenant_id=c.tenant_id AND s.contract_id=c.id)").Order("c.id").Limit(200).Scan(&rows).Error; err != nil {
			return count, err
		}
		if len(rows) == 0 {
			return count, nil
		}
		for _, row := range rows {
			err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				var current contractRecord
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", row.TenantID, row.ContractID).Take(&current).Error; err != nil {
					return err
				}
				return enqueueContractResponsibility(tx, row.TenantID, row.ContractID)
			})
			if err != nil {
				return count, err
			}
			count++
		}
	}
}

// ClaimContractResponsibilityDelivery 认领到期责任事件并回收过期租约。
// 参数：ctx 控制认领事务及租约回收。
// 返回值：认领载荷、是否存在和存储错误。
func (r *Repository) ClaimContractResponsibilityDelivery(ctx context.Context) (settlementintegration.Delivery, bool, error) {
	var delivery settlementintegration.Delivery
	found := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row contractResponsibilityOutbox
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
		if err := tx.Model(&contractResponsibilityOutbox{}).Where("id = ?", row.ID).
			Updates(map[string]any{"delivery_status": "processing", "attempts": attempts, "locked_at": now}).Error; err != nil {
			return err
		}
		delivery = settlementintegration.Delivery{ID: row.ID, TenantID: row.TenantID, Payload: row.PayloadJSON, Attempts: attempts}
		found = true
		return nil
	})
	return delivery, found, err
}

// MarkContractResponsibilityDelivered 记录责任事件已被结算接受。
// 参数：ctx 控制存储操作，id 为已认领责任队列标识。
// 返回值：存储失败或队列状态冲突错误。
func (r *Repository) MarkContractResponsibilityDelivered(ctx context.Context, id string) error {
	now := time.Now().UTC()
	result := r.db.WithContext(ctx).Model(&contractResponsibilityOutbox{}).
		Where("id = ? AND delivery_status = ?", id, "processing").
		Updates(map[string]any{"delivery_status": "delivered", "delivered_at": now, "locked_at": nil, "last_error": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// MarkContractResponsibilityFailed 持久记录责任事件重试退避或死信。
// 参数：ctx 控制写入，id 为队列号，message 为摘要，attempts 为次数，dead 标记死信。
// 返回值：持久化失败错误。
func (r *Repository) MarkContractResponsibilityFailed(ctx context.Context, id, message string, attempts uint, dead bool) error {
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
	return r.db.WithContext(ctx).Model(&contractResponsibilityOutbox{}).
		Where("id = ? AND delivery_status = ?", id, "processing").
		Updates(map[string]any{"delivery_status": status, "next_attempt_at": time.Now().UTC().Add(delay), "locked_at": nil, "last_error": message}).Error
}
