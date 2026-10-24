package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type templateFileOutboxRecord struct {
	ID                                                               string `gorm:"primaryKey"`
	TenantID, TemplateID, FileID, Filename, ResourceType, ResourceID string
	Document                                                         []byte `gorm:"type:longblob"`
	Retired                                                          bool
	DeliveryStatus                                                   string
	Attempts                                                         uint64
	UploadAttempt                                                    uint64
	UploadFailuresJSON                                               []byte `gorm:"type:json"`
	NextAttemptAt                                                    time.Time
	LastError                                                        string
	CreatedAt                                                        time.Time
}

func (templateFileOutboxRecord) TableName() string { return "con_template_file_outbox" }

func (r *Repository) CreateTemplateWithFile(ctx context.Context, item contracttemplate.Template) error {
	item.PlatformFileID, item.FileGatewayState = "", "PENDING"
	return r.CreateTemplate(ctx, item)
}
func (r *Repository) UpdateTemplateSourceWithFile(ctx context.Context, item contracttemplate.Template) error {
	item.PlatformFileID, item.FileGatewayState = "", "PENDING"
	return r.UpdateTemplateSource(ctx, item)
}

// QueueTemplateFileBackfill 持锁读取当前源。替换或删除期间，
// CLI 不得上传更早的快照。
func (r *Repository) QueueTemplateFileBackfill(ctx context.Context, tenantID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractTemplateRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, id).Take(&current).Error; err != nil {
			return err
		}
		if current.GatewaySourceID != "" {
			return nil
		}
		sourceID, err := enqueueTemplateSource(tx, templateFromRecord(current))
		if err != nil {
			return err
		}
		if current.PlatformFileID != "" {
			if err := tx.Model(&templateFileOutboxRecord{}).Where("id = ?", sourceID).Updates(map[string]any{"file_id": current.PlatformFileID, "resource_type": "contract_template", "resource_id": current.ID}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&contractTemplateRecord{}).Where("tenant_id = ? AND id = ?", tenantID, id).Updates(map[string]any{"gateway_source_id": sourceID, "file_gateway_state": "PENDING"}).Error
	})
}

func enqueueTemplateSource(tx *gorm.DB, item contracttemplate.Template) (string, error) {
	id := newID()
	now := time.Now().UTC()
	err := tx.Create(&templateFileOutboxRecord{ID: id, TenantID: item.TenantID, TemplateID: item.ID, Filename: item.OriginalFilename, ResourceType: "contract_template_source", ResourceID: id, Document: item.Content, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now}).Error
	return id, err
}

func retireTemplateSource(tx *gorm.DB, current contractTemplateRecord) error {
	if current.GatewaySourceID != "" {
		result := tx.Model(&templateFileOutboxRecord{}).Where("tenant_id = ? AND id = ?", current.TenantID, current.GatewaySourceID).
			Updates(map[string]any{"retired": true, "delivery_status": "pending", "next_attempt_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("template source lifecycle record is missing")
		}
		return nil
	}
	// 同时处理迁移后由旧版回填工具写入的源。
	if current.PlatformFileID == "" {
		return nil
	}
	now := time.Now().UTC()
	return tx.Create(&templateFileOutboxRecord{ID: newID(), TenantID: current.TenantID, TemplateID: current.ID, FileID: current.PlatformFileID, Filename: current.OriginalFilename, ResourceType: "contract_template", ResourceID: current.ID, Document: current.Document, Retired: true, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now}).Error
}

// 不使用可过期租约：有界网关调用与退役操作使用
// 同一源行锁。因此慢工作器不能在其他工作器
// 已确认释放之后，再绑定已退役源。
func (r *Repository) ProcessTemplateFile(ctx context.Context, tenantID string, deliver func(context.Context, application.TemplateFileDelivery) (string, error)) (bool, error) {
	if tenantID == "" {
		return false, application.ErrForbidden
	}
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	var candidate templateFileOutboxRecord
	result := r.db.WithContext(ctx).Select("id", "tenant_id", "template_id").Where("tenant_id = ? AND delivery_status = ? AND next_attempt_at <= ?", tenantID, "pending", time.Now().UTC()).Order("retired DESC, next_attempt_at, created_at, id").Limit(1).Find(&candidate)
	if result.Error != nil || result.RowsAffected == 0 {
		return false, result.Error
	}
	var deliveryErr error
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 遵循变更锁顺序：先模板，再不可变源。
		var template contractTemplateRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", candidate.TenantID, candidate.TemplateID).Take(&template).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var source templateFileOutboxRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, candidate.ID).Take(&source).Error; err != nil {
			return err
		}
		if source.DeliveryStatus != "pending" || source.NextAttemptAt.After(time.Now().UTC()) {
			return nil
		}
		if !source.Retired && template.GatewaySourceID != source.ID {
			return apperrors.ErrVersionConflict
		}
		fileID, callErr := deliver(ctx, application.TemplateFileDelivery{ID: source.ID, TenantID: source.TenantID, TemplateID: source.TemplateID, FileID: source.FileID, Filename: source.Filename, Content: source.Document, ResourceType: source.ResourceType, ResourceID: source.ResourceID, Retired: source.Retired, UploadAttempt: source.UploadAttempt})
		updates := map[string]any{"attempts": source.Attempts + 1, "delivery_status": "delivered", "last_error": ""}
		state := "READY"
		if fileID != "" {
			updates["file_id"] = fileID
		}
		if callErr != nil {
			delay := time.Second * time.Duration(1<<min(source.Attempts, 9))
			updates["delivery_status"], updates["last_error"], updates["next_attempt_at"] = "pending", "GATEWAY_DELIVERY_FAILED", time.Now().UTC().Add(delay)
			state = "FAILED"
			deliveryErr = callErr
			if errors.Is(callErr, application.ErrTemplateUploadFailed) && !source.Retired {
				var history []struct {
					Attempt uint64 `json:"attempt"`
					FileID  string `json:"file_id"`
				}
				if len(source.UploadFailuresJSON) > 0 {
					if err := json.Unmarshal(source.UploadFailuresJSON, &history); err != nil {
						return err
					}
				}
				history = append(history, struct {
					Attempt uint64 `json:"attempt"`
					FileID  string `json:"file_id"`
				}{source.UploadAttempt, fileID})
				encoded, err := json.Marshal(history)
				if err != nil {
					return err
				}
				updates["upload_failures_json"], updates["upload_attempt"], updates["file_id"] = encoded, source.UploadAttempt+1, ""
				updates["last_error"] = "UPLOAD_ATTEMPT_FAILED"
				fileID = ""
			}
			if errors.Is(callErr, application.ErrTemplateUploadRejected) {
				updates["delivery_status"], updates["last_error"] = "rejected", "UPLOAD_CONTENT_REJECTED"
			}
		}
		if err := tx.Model(&templateFileOutboxRecord{}).Where("id = ?", source.ID).Updates(updates).Error; err != nil {
			return err
		}
		if !source.Retired {
			// 上方已校验锁定身份。重复投递失败时，
			// 未改变的 FAILED 结果合法地影响零条 MySQL 行。
			result := tx.Model(&contractTemplateRecord{}).Where("tenant_id = ? AND id = ? AND gateway_source_id = ?", source.TenantID, source.TemplateID, source.ID).Updates(map[string]any{"platform_file_id": fileID, "file_gateway_state": state, "file_gateway_last_error": updates["last_error"]})
			if result.Error != nil {
				return result.Error
			}
		}
		return nil
	})
	if err != nil {
		return true, err
	}
	return true, deliveryErr
}
