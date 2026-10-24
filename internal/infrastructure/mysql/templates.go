package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) CreateTemplate(ctx context.Context, item contracttemplate.Template) error {
	fields, err := json.Marshal(item.Fields)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := contractTemplateRecord{
		ID: item.ID, TenantID: item.TenantID, Name: item.Name,
		OriginalFilename: item.OriginalFilename, NumberFormat: item.NumberFormat, FieldsJSON: fields,
		Document: item.Content, PlatformFileID: item.PlatformFileID, FileGatewayState: item.FileGatewayState,
		Version: 1, CreatedAt: now, CreatedBy: item.CreatedBy,
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if item.FileGatewayState == "PENDING" && item.PlatformFileID == "" {
			id, err := enqueueTemplateSource(tx, item)
			if err != nil {
				return err
			}
			record.GatewaySourceID = id
		}
		return tx.Create(&record).Error
	})
}

func (r *Repository) ListTemplates(ctx context.Context, tenantID string) ([]contracttemplate.Template, error) {
	var records []contractTemplateRecord
	if err := r.db.WithContext(ctx).
		Select("id", "tenant_id", "name", "original_filename", "number_format", "fields_json", "version", "created_at", "created_by").
		Where("tenant_id = ?", tenantID).Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contracttemplate.Template, 0, len(records))
	for _, record := range records {
		item := templateFromRecord(record)
		if err := json.Unmarshal(record.FieldsJSON, &item.Fields); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *Repository) GetTemplate(ctx context.Context, tenantID, id string) (contracttemplate.Template, error) {
	var record contractTemplateRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contracttemplate.Template{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contracttemplate.Template{}, err
	}
	item := templateFromRecord(record)
	if err := json.Unmarshal(record.FieldsJSON, &item.Fields); err != nil {
		return contracttemplate.Template{}, err
	}
	return item, nil
}

// UpdateTemplate 只更新模板元数据，绝不回写 document/platform_file_id 等源文件字段。
// 并发替换源文件后，旧元数据请求会因版本不符而失败，而不是把新正文覆盖回旧版本。
func (r *Repository) UpdateTemplate(ctx context.Context, item contracttemplate.Template) error {
	fields, err := json.Marshal(item.Fields)
	if err != nil {
		return err
	}
	result := r.db.WithContext(ctx).Model(&contractTemplateRecord{}).
		Where("tenant_id = ? AND id = ? AND version = ?", item.TenantID, item.ID, item.Version).
		Updates(map[string]any{
			"name": item.Name, "number_format": item.NumberFormat, "fields_json": fields,
			"version": gorm.Expr("version + 1"),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrVersionConflict
	}
	return nil
}

// UpdateTemplateSource 只更新源文件与字段投影；名称/编号格式等元数据由 UpdateTemplate 负责。
func (r *Repository) UpdateTemplateSource(ctx context.Context, item contracttemplate.Template) error {
	fields, err := json.Marshal(item.Fields)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractTemplateRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", item.TenantID, item.ID).Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrVersionConflict
			}
			return err
		}
		if current.Version != item.Version {
			return apperrors.ErrVersionConflict
		}
		if err := retireTemplateSource(tx, current); err != nil {
			return err
		}
		sourceID := ""
		if item.FileGatewayState == "PENDING" && item.PlatformFileID == "" {
			var err error
			sourceID, err = enqueueTemplateSource(tx, item)
			if err != nil {
				return err
			}
		}
		result := tx.Model(&contractTemplateRecord{}).
			Where("tenant_id = ? AND id = ? AND version = ?", item.TenantID, item.ID, item.Version).
			Updates(map[string]any{
				"original_filename": item.OriginalFilename, "document": item.Content, "fields_json": fields,
				"platform_file_id": item.PlatformFileID, "file_gateway_state": item.FileGatewayState,
				"file_gateway_last_error": "", "gateway_source_id": sourceID,
				"version": gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrVersionConflict
		}
		return nil
	})
}

func (r *Repository) DeleteTemplate(ctx context.Context, tenantID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractTemplateRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, id).Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			return err
		}
		if err := retireTemplateSource(tx, current); err != nil {
			return err
		}
		return tx.Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&contractTemplateRecord{}).Error
	})
}
