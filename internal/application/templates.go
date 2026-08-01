package application

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/docx"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/oklog/ulid/v2"
)

type TemplateRepository interface {
	CreateTemplate(context.Context, contracttemplate.Template) error
	ListTemplates(context.Context, string) ([]contracttemplate.Template, error)
	GetTemplate(context.Context, string, string) (contracttemplate.Template, error)
	UpdateTemplate(context.Context, contracttemplate.Template) error
	DeleteTemplate(context.Context, string, string) error
}

func (s *Service) CreateTemplate(ctx context.Context, actor Principal, name, filename string, content []byte) (contracttemplate.Template, error) {
	if !hasRole(actor, "admin") {
		return contracttemplate.Template{}, ErrForbidden
	}
	name, filename = strings.TrimSpace(name), filepath.Base(strings.TrimSpace(filename))
	if s.Templates == nil {
		return contracttemplate.Template{}, fmt.Errorf("template repository is not configured")
	}
	if name == "" {
		return contracttemplate.Template{}, fmt.Errorf("%w: 模板名称不能为空", ErrValidation)
	}
	if filename == "." || !strings.EqualFold(filepath.Ext(filename), ".docx") {
		return contracttemplate.Template{}, fmt.Errorf("%w: 仅支持 .docx 模板文件", ErrValidation)
	}
	fields, err := docx.Fields(content)
	if err != nil {
		return contracttemplate.Template{}, fmt.Errorf("%w: DOCX 模板解析失败：%v", ErrValidation, err)
	}
	item := contracttemplate.Template{
		ID: ulid.Make().String(), TenantID: actor.TenantID, Name: name,
		OriginalFilename: filename, NumberFormat: contracttemplate.DefaultNumberFormat, Fields: fields, Content: append([]byte(nil), content...),
		CreatedAt: time.Now().UTC(), CreatedBy: actor.UserID,
	}
	if err := s.Templates.CreateTemplate(ctx, item); err != nil {
		return contracttemplate.Template{}, err
	}
	return item, nil
}

func (s *Service) ListTemplates(ctx context.Context, actor Principal) ([]contracttemplate.Template, error) {
	if !actor.Has("contract.create") && !hasRole(actor, "admin") {
		return nil, ErrForbidden
	}
	if s.Templates == nil {
		return nil, nil
	}
	return s.Templates.ListTemplates(ctx, actor.TenantID)
}

func (s *Service) UpdateTemplate(ctx context.Context, actor Principal, id, name, numberFormat string, fields []contracttemplate.Field) (contracttemplate.Template, error) {
	if !hasRole(actor, "admin") {
		return contracttemplate.Template{}, ErrForbidden
	}
	if s.Templates == nil {
		return contracttemplate.Template{}, fmt.Errorf("template repository is not configured")
	}
	item, err := s.Templates.GetTemplate(ctx, actor.TenantID, strings.TrimSpace(id))
	if err != nil {
		return contracttemplate.Template{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return contracttemplate.Template{}, fmt.Errorf("%w: 模板名称不能为空", ErrValidation)
	}
	numberFormat, err = normalizeNumberFormat(numberFormat)
	if err != nil {
		return contracttemplate.Template{}, err
	}
	if len(fields) != len(item.Fields) {
		return contracttemplate.Template{}, fmt.Errorf("%w: 模板字段不能增加或删除", ErrValidation)
	}
	configured := make(map[string]contracttemplate.Field, len(fields))
	for _, field := range fields {
		field.Name = strings.TrimSpace(field.Name)
		field.Label = strings.TrimSpace(field.Label)
		field.Default = strings.TrimSpace(field.Default)
		if field.Name == "" || field.Label == "" {
			return contracttemplate.Template{}, fmt.Errorf("%w: 字段名称和显示名称不能为空", ErrValidation)
		}
		if _, exists := configured[field.Name]; exists {
			return contracttemplate.Template{}, fmt.Errorf("%w: 模板字段重复：%s", ErrValidation, field.Name)
		}
		if field.Locked && field.Default == "" {
			return contracttemplate.Template{}, fmt.Errorf("%w: 管理员配置字段“%s”必须填写固定值", ErrValidation, field.Label)
		}
		configured[field.Name] = field
	}
	updatedFields := make([]contracttemplate.Field, 0, len(item.Fields))
	for _, existing := range item.Fields {
		field, exists := configured[existing.Name]
		if !exists {
			return contracttemplate.Template{}, fmt.Errorf("%w: 模板字段不存在：%s", ErrValidation, existing.Name)
		}
		updatedFields = append(updatedFields, field)
	}
	item.Name, item.NumberFormat, item.Fields = name, numberFormat, updatedFields
	if err := s.Templates.UpdateTemplate(ctx, item); err != nil {
		return contracttemplate.Template{}, err
	}
	return item, nil
}

func normalizeNumberFormat(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > 160 {
		return "", fmt.Errorf("%w: 合同编号格式不能为空且不能超过160个字符", ErrValidation)
	}
	if !strings.Contains(value, "{ID8}") {
		return "", fmt.Errorf("%w: 合同编号格式必须包含唯一标识 {ID8}", ErrValidation)
	}
	remaining := strings.NewReplacer("{YYYYMMDD}", "", "{YYYY}", "", "{MM}", "", "{DD}", "", "{ID8}", "").Replace(value)
	if strings.ContainsAny(remaining, "{}") {
		return "", fmt.Errorf("%w: 合同编号格式包含不支持的占位符", ErrValidation)
	}
	return value, nil
}

func (s *Service) DeleteTemplate(ctx context.Context, actor Principal, id string) error {
	if !hasRole(actor, "admin") {
		return ErrForbidden
	}
	if s.Templates == nil {
		return fmt.Errorf("template repository is not configured")
	}
	return s.Templates.DeleteTemplate(ctx, actor.TenantID, strings.TrimSpace(id))
}

func (s *Service) PreviewTemplate(ctx context.Context, actor Principal, id string, values map[string]string) (string, error) {
	rendered, _, err := s.renderTemplate(ctx, actor, id, values)
	if err != nil {
		return "", err
	}
	preview, err := docx.PreviewHTML(rendered)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return preview, nil
}

func (s *Service) renderTemplate(ctx context.Context, actor Principal, id string, values map[string]string) ([]byte, map[string]string, error) {
	if !actor.Has("contract.create") || s.Templates == nil || strings.TrimSpace(id) == "" {
		return nil, nil, ErrForbidden
	}
	item, err := s.Templates.GetTemplate(ctx, actor.TenantID, id)
	if err != nil {
		return nil, nil, err
	}
	normalized, err := normalizeTemplateValues(item.Fields, values, hasRole(actor, "admin"))
	if err != nil {
		return nil, nil, err
	}
	rendered, err := docx.Render(item.Content, normalized)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return rendered, normalized, nil
}

func normalizeTemplateValues(fields []contracttemplate.Field, values map[string]string, admin bool) (map[string]string, error) {
	expected := make(map[string]bool, len(fields))
	normalized := make(map[string]string, len(fields))
	for _, field := range fields {
		expected[field.Name] = true
		value, supplied := values[field.Name]
		if field.Locked && !admin {
			if supplied && value != field.Default {
				return nil, fmt.Errorf("%w: 字段“%s”由管理员配置，不能修改", ErrValidation, field.Label)
			}
			normalized[field.Name] = field.Default
			continue
		}
		if !supplied {
			value = field.Default
		}
		if strings.TrimSpace(value) == "" && field.Default == "" {
			return nil, fmt.Errorf("%w: missing template field %s", ErrValidation, field.Name)
		}
		normalized[field.Name] = value
	}
	for name := range values {
		if !expected[name] {
			return nil, fmt.Errorf("%w: unknown template field %s", ErrValidation, name)
		}
	}
	return normalized, nil
}

func hasRole(actor Principal, expected string) bool {
	for _, role := range actor.Roles {
		if role == expected {
			return true
		}
	}
	return false
}
