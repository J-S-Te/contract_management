package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type importBatchRecord struct {
	ID            string
	TenantID      string
	ActorID       string
	State         string
	Version       uint64
	Payload       []byte `gorm:"type:json"`
	CommitKeyHash string
	SessionHash   []byte
	LeaseUntil    *time.Time
	UpdatedAt     time.Time
}

// TableName 指定批次权威台账。
// 参数：无。
// 返回值：固定表名。
func (importBatchRecord) TableName() string { return "con_import_batch" }

type importIdentityRecord struct {
	TenantID         string
	SourceSystem     string
	ExternalRecordID string
	Digest           string
	ContractID       *string
	ItemID           string
	CreatedAt        time.Time
}

// TableName 指定来源业务键映射表。
// 参数：无。
// 返回值：固定表名。
func (importIdentityRecord) TableName() string { return "con_import_identity" }

// decodeImportBatch 恢复载荷及不对浏览器公开的执行会话。
// 参数：row为持久台账。
// 返回值：批次或JSON损坏错误。
func decodeImportBatch(row importBatchRecord) (contract.ImportBatch, error) {
	var b contract.ImportBatch
	err := json.Unmarshal(row.Payload, &b)
	b.TenantID, b.SessionHash, b.CommitKeyHash = row.TenantID, row.SessionHash, row.CommitKeyHash
	if row.LeaseUntil != nil {
		b.LeaseUntil = *row.LeaseUntil
	}
	return b, err
}

// saveImportBatch 在当前事务内保存台账版本及执行围栏。
// 参数：tx为已持有行锁的事务，b为待写批次。
// 返回值：存储或序列化错误。
func saveImportBatch(tx *gorm.DB, b *contract.ImportBatch) error {
	b.Version++
	payload, err := json.Marshal(b)
	if err != nil {
		return err
	}
	var lease *time.Time
	if !b.LeaseUntil.IsZero() {
		lease = &b.LeaseUntil
	}
	return tx.Model(&importBatchRecord{}).Where("tenant_id=? AND id=?", b.TenantID, b.ID).Updates(map[string]any{
		"payload": payload, "state": b.State, "version": b.Version, "session_hash": b.SessionHash,
		"commit_key_hash": b.CommitKeyHash, "lease_until": lease, "updated_at": time.Now().UTC()}).Error
}

// CreateImportBatch 保存没有业务合同副作用的空批次。
// 参数：ctx为上下文，b为已分配身份的批次。
// 返回值：台账创建错误。
func (r *Repository) CreateImportBatch(ctx context.Context, b contract.ImportBatch) error {
	payload, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(&importBatchRecord{ID: b.ID, TenantID: b.TenantID, ActorID: b.ActorID,
		State: b.State, Version: b.Version, Payload: payload, UpdatedAt: time.Now().UTC()}).Error
}

// GetImportBatch 仅按可信租户读取一份批次。
// 参数：ctx为上下文，tenant和id限定记录身份。
// 返回值：批次、不存在或数据库错误。
func (r *Repository) GetImportBatch(ctx context.Context, tenant, id string) (contract.ImportBatch, error) {
	var row importBatchRecord
	err := r.db.WithContext(ctx).Where("tenant_id=? AND id=?", tenant, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.ImportBatch{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.ImportBatch{}, err
	}
	return decodeImportBatch(row)
}

// ListImportCleanup 读取到期且无在途业务的有限暂存批次。
// 参数：ctx为上下文，now为当前UTC时间，预留两分钟上传完成宽限。
// 返回值：最多十份台账或读取错误。
func (r *Repository) ListImportCleanup(ctx context.Context, now time.Time) ([]contract.ImportBatch, error) {
	var rows []importBatchRecord
	err := r.db.WithContext(ctx).Where("state IN ('DRAFT','BLOCKED','EXPIRED','COMPLETED') AND updated_at < ?", now.Add(-2*time.Minute)).
		Where("JSON_LENGTH(JSON_EXTRACT(payload,'$.sources')) > 0").
		Where("STR_TO_DATE(SUBSTRING(JSON_UNQUOTE(JSON_EXTRACT(payload,'$.expires_at')),1,19),'%Y-%m-%dT%H:%i:%s') < ?", now).
		Where("JSON_SEARCH(payload,'one','CLEANED',NULL,'$.sources[*].state') IS NULL OR JSON_SEARCH(payload,'one','READY',NULL,'$.sources[*].state') IS NOT NULL OR JSON_SEARCH(payload,'one','UPLOADING',NULL,'$.sources[*].state') IS NOT NULL").
		Order("updated_at,id").Limit(10).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make([]contract.ImportBatch, 0, len(rows))
	for _, row := range rows {
		b, e := decodeImportBatch(row)
		if e != nil {
			return nil, e
		}
		if len(b.Sources) > 0 {
			result = append(result, b)
		}
	}
	return result, nil
}

// UpdateImportBatch 在短事务行锁内校验并更新批次。
// 参数：ctx、tenant、id限定批次，change只处理本地元数据不得调用远端。
// 返回值：冲突、修改回调或数据库错误。
func (r *Repository) UpdateImportBatch(ctx context.Context, tenant, id string, change func(*contract.ImportBatch) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, id).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		b, err := decodeImportBatch(row)
		if err != nil {
			return err
		}
		if err = change(&b); err != nil {
			return err
		}
		return saveImportBatch(tx, &b)
	})
}

// ClaimImportBatch 领取一个可恢复任务并递增执行代次。
// 参数：ctx为上下文，now为当前UTC时间。
// 返回值：具有两分钟租约的批次、是否找到及数据库错误。
func (r *Repository) ClaimImportBatch(ctx context.Context, now time.Time) (b contract.ImportBatch, found bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		e := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("state IN ('QUEUED','RUNNING') AND (lease_until IS NULL OR lease_until < ?)", now).Order("updated_at,id").Take(&row).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return nil
		}
		if e != nil {
			return e
		}
		b, e = decodeImportBatch(row)
		if e != nil {
			return e
		}
		b.State = "RUNNING"
		b.Epoch++
		b.LeaseUntil = now.Add(2 * time.Minute)
		found = true
		return saveImportBatch(tx, &b)
	})
	return
}

// CommitImportDraft 在同一事务内创建合同、来源身份及单项结果。
// 参数：ctx限定执行，tenant/batch/revision/epoch为围栏，itemID及c为已重新校验的基线，actor为真实操作者。
// 返回值：稳定合同ID；来源冲突、旧代次或创建失败均整体回滚。
func (r *Repository) CommitImportDraft(ctx context.Context, tenant, batch string, revision, epoch uint64, itemID string, c contract.Contract, actor string) (id string, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, batch).Take(&row).Error; e != nil {
			return e
		}
		b, e := decodeImportBatch(row)
		if e != nil {
			return e
		}
		if b.Revision != revision || b.Epoch != epoch || b.State != "RUNNING" || !b.LeaseUntil.After(time.Now().UTC()) {
			return application.ErrImportConflict
		}
		for i := range b.Items {
			if b.Items[i].ID == itemID {
				item := &b.Items[i]
				if item.ContractID != "" {
					id = item.ContractID
					return nil
				}
				identity := importIdentityRecord{TenantID: tenant, SourceSystem: b.SourceSystem, ExternalRecordID: item.Item.ExternalRecordID, Digest: item.Digest, ItemID: itemID, CreatedAt: time.Now().UTC()}
				if e = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&identity).Error; e != nil {
					return e
				}
				if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND source_system=? AND external_record_id=?", tenant, b.SourceSystem, item.Item.ExternalRecordID).Take(&identity).Error; e != nil {
					return e
				}
				if identity.Digest != item.Digest {
					return application.ErrImportIdentityConflict
				}
				if identity.ContractID != nil {
					id = *identity.ContractID
				} else {
					nested := *r
					nested.db = tx
					if e = nested.CreateContract(ctx, c, actor); e != nil {
						return e
					}
					id = c.ID
					if e = tx.Model(&importIdentityRecord{}).Where("tenant_id=? AND source_system=? AND external_record_id=?", tenant, b.SourceSystem, item.Item.ExternalRecordID).Update("contract_id", id).Error; e != nil {
						return e
					}
				}
				item.ContractID, item.State = id, "CREATED"
				item.Issues = nil
				return saveImportBatch(tx, &b)
			}
		}
		return application.ErrImportConflict
	})
	return
}
