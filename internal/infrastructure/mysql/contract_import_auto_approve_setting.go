package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// importAutoApproveSettingAuditAction 是运行开关变更的事务内审计动作。
// 与自动确认本身（IMPORT_AUTO_APPROVED）分开：这里只记录「谁把开关改成什么」，
// 不写任何客户、原件或清单内容。
const importAutoApproveSettingAuditAction = "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVE_SETTING_CHANGED"

// importAutoApproveSettingRecord 是租户级历史导入自动审批运行开关行。
type importAutoApproveSettingRecord struct {
	TenantID  string    `gorm:"column:tenant_id;primaryKey"`
	Enabled   bool      `gorm:"column:enabled"`
	Version   uint64    `gorm:"column:version"`
	UpdatedBy string    `gorm:"column:updated_by"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (importAutoApproveSettingRecord) TableName() string { return "con_import_auto_approve_setting" }

// importAutoApproveSettingFromRecord 把持久化行投影成领域结构。
// 参数：record 为已读取的设置行。
// 返回值：领域设置。
func importAutoApproveSettingFromRecord(record importAutoApproveSettingRecord) contract.ImportAutoApproveSetting {
	return contract.ImportAutoApproveSetting{
		TenantID: record.TenantID, Enabled: record.Enabled, Version: record.Version,
		UpdatedBy: record.UpdatedBy, UpdatedAt: record.UpdatedAt,
	}
}

// GetImportAutoApproveSetting 读取租户级自动审批运行开关。
// 参数：ctx 为请求上下文，tenantID 为租户。
// 返回值：无记录时返回零值（Version=0）且不报错，供接口表达「默认关闭」。
func (r *Repository) GetImportAutoApproveSetting(ctx context.Context, tenantID string) (contract.ImportAutoApproveSetting, error) {
	var record importAutoApproveSettingRecord
	err := r.db.WithContext(ctx).Where("tenant_id=?", tenantID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.ImportAutoApproveSetting{}, nil
	}
	if err != nil {
		return contract.ImportAutoApproveSetting{}, err
	}
	return importAutoApproveSettingFromRecord(record), nil
}

// SetImportAutoApproveSetting 以乐观版本写入租户级自动审批运行开关。
//
// 并发控制：事务内先 SELECT ... FOR UPDATE 锁住租户行；无行时 expected_version 必须为 0，
// 插入 version=1；有行时 expected_version 必须等于当前 version，写入 enabled/version+1/
// updated_by/updated_at。版本不符统一返回 application.ErrImportConflict（HTTP 409）。
// 同事务写审计 outbox：开关变更与业务状态同提交，投递由后台 dispatcher 完成。
//
// 参数：ctx 为请求上下文，tenantID 为租户，enabled 为目标状态，expectedVersion 为客户端
// 读到的版本（首次写入为 0），actorID 为操作者（写入 updated_by 与审计 ActorID）。
// 返回值：写入后的设置；版本冲突、并发首写或存储错误。
func (r *Repository) SetImportAutoApproveSetting(ctx context.Context, tenantID string, enabled bool, expectedVersion uint64, actorID string) (contract.ImportAutoApproveSetting, error) {
	var setting contract.ImportAutoApproveSetting
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record importAutoApproveSettingRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=?", tenantID).Take(&record).Error
		now := time.Now().UTC()
		// previousEnabled 只用于审计：无记录时按「关闭」处理。
		previousEnabled := false
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			// 首次写入只接受 expected_version=0，避免客户端把「没有记录」误当成一个版本号。
			if expectedVersion != 0 {
				return application.ErrImportConflict
			}
			record = importAutoApproveSettingRecord{TenantID: tenantID, Enabled: enabled, Version: 1, UpdatedBy: actorID, UpdatedAt: now}
			if createErr := tx.Create(&record).Error; createErr != nil {
				// 并发首写：唯一键冲突等价于版本已被别人推进，按冲突交给客户端重读。
				if isImportAutoApproveDuplicate(createErr) {
					return application.ErrImportConflict
				}
				return createErr
			}
		case err != nil:
			return err
		default:
			if expectedVersion != record.Version {
				return application.ErrImportConflict
			}
			previousEnabled = record.Enabled
			record.Enabled = enabled
			record.Version++
			record.UpdatedBy = actorID
			record.UpdatedAt = now
			updated := tx.Model(&importAutoApproveSettingRecord{}).
				Where("tenant_id=? AND version=?", tenantID, expectedVersion).
				Updates(map[string]any{
					"enabled":    record.Enabled,
					"version":    record.Version,
					"updated_by": record.UpdatedBy,
					"updated_at": record.UpdatedAt,
				})
			if updated.Error != nil {
				return updated.Error
			}
			// 行锁下版本不会漂移；这里仍校验影响行数，避免未来改动把 CAS 变成盲写。
			if updated.RowsAffected != 1 {
				return application.ErrImportConflict
			}
		}
		// 审计只记录开关前后状态与版本：不写令牌、客户名或任何清单正文。
		payload, marshalErr := json.Marshal(map[string]any{
			"enabled":          enabled,
			"previous_enabled": previousEnabled,
			"version":          record.Version,
		})
		if marshalErr != nil {
			return marshalErr
		}
		if auditErr := insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
			ActorID:      actorID,
			Action:       importAutoApproveSettingAuditAction,
			ResourceType: "contract_import_auto_approve",
			ResourceID:   tenantID,
			Payload:      payload,
		}); auditErr != nil {
			return auditErr
		}
		setting = importAutoApproveSettingFromRecord(record)
		return nil
	})
	if err != nil {
		return contract.ImportAutoApproveSetting{}, err
	}
	return setting, nil
}

// isImportAutoApproveDuplicate 识别唯一键冲突（1062），用于并发首写。
// 与合同编号的同类判断一致：GORM TranslateError 打开时是 ErrDuplicatedKey，
// 关闭时是驱动错误文本。
// 参数：err 为插入失败错误。
// 返回值：可判定为唯一键重复时为 true。
func isImportAutoApproveDuplicate(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
