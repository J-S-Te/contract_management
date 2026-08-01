package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"gorm.io/gorm"
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
		Document: item.Content, CreatedAt: now, CreatedBy: item.CreatedBy,
	}
	return r.db.WithContext(ctx).Create(&record).Error
}

func (r *Repository) ListTemplates(ctx context.Context, tenantID string) ([]contracttemplate.Template, error) {
	var records []contractTemplateRecord
	if err := r.db.WithContext(ctx).
		Select("id", "tenant_id", "name", "original_filename", "number_format", "fields_json", "created_at", "created_by").
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

func (r *Repository) UpdateTemplate(ctx context.Context, item contracttemplate.Template) error {
	fields, err := json.Marshal(item.Fields)
	if err != nil {
		return err
	}
	result := r.db.WithContext(ctx).Model(&contractTemplateRecord{}).
		Where("tenant_id = ? AND id = ?", item.TenantID, item.ID).
		Updates(map[string]any{"name": item.Name, "number_format": item.NumberFormat, "fields_json": fields})
	if result.Error != nil {
		return result.Error
	}
	return nil
}

func (r *Repository) DeleteTemplate(ctx context.Context, tenantID, id string) error {
	result := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&contractTemplateRecord{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return apperrors.ErrNotFound
	}
	return nil
}
