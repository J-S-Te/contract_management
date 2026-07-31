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
}

func (s *Service) CreateTemplate(ctx context.Context, actor Principal, name, filename string, content []byte) (contracttemplate.Template, error) {
	if !actor.Has("contract_template.manage") || !hasRole(actor, "admin") {
		return contracttemplate.Template{}, ErrForbidden
	}
	name, filename = strings.TrimSpace(name), filepath.Base(strings.TrimSpace(filename))
	if s.Templates == nil || name == "" || filename == "." || !strings.EqualFold(filepath.Ext(filename), ".docx") {
		return contracttemplate.Template{}, ErrValidation
	}
	fields, err := docx.Fields(content)
	if err != nil {
		return contracttemplate.Template{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	item := contracttemplate.Template{
		ID: ulid.Make().String(), TenantID: actor.TenantID, Name: name,
		OriginalFilename: filename, Fields: fields, Content: append([]byte(nil), content...),
		CreatedAt: time.Now().UTC(), CreatedBy: actor.UserID,
	}
	if err := s.Templates.CreateTemplate(ctx, item); err != nil {
		return contracttemplate.Template{}, err
	}
	return item, nil
}

func (s *Service) ListTemplates(ctx context.Context, actor Principal) ([]contracttemplate.Template, error) {
	if !actor.Has("contract.create") && !actor.Has("contract_template.manage") {
		return nil, ErrForbidden
	}
	if s.Templates == nil {
		return nil, nil
	}
	return s.Templates.ListTemplates(ctx, actor.TenantID)
}

func (s *Service) PreviewTemplate(ctx context.Context, actor Principal, id string, values map[string]string) (string, error) {
	rendered, err := s.renderTemplate(ctx, actor, id, values)
	if err != nil {
		return "", err
	}
	preview, err := docx.PreviewHTML(rendered)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return preview, nil
}

func (s *Service) renderTemplate(ctx context.Context, actor Principal, id string, values map[string]string) ([]byte, error) {
	if !actor.Has("contract.create") || s.Templates == nil || strings.TrimSpace(id) == "" {
		return nil, ErrForbidden
	}
	item, err := s.Templates.GetTemplate(ctx, actor.TenantID, id)
	if err != nil {
		return nil, err
	}
	if err := validateTemplateValues(item.Fields, values); err != nil {
		return nil, err
	}
	rendered, err := docx.Render(item.Content, values)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	return rendered, nil
}

func validateTemplateValues(fields []contracttemplate.Field, values map[string]string) error {
	expected := make(map[string]bool, len(fields))
	for _, field := range fields {
		expected[field.Name] = true
		if _, ok := values[field.Name]; !ok {
			return fmt.Errorf("%w: missing template field %s", ErrValidation, field.Name)
		}
	}
	for name := range values {
		if !expected[name] {
			return fmt.Errorf("%w: unknown template field %s", ErrValidation, name)
		}
	}
	return nil
}

func hasRole(actor Principal, expected string) bool {
	for _, role := range actor.Roles {
		if role == expected {
			return true
		}
	}
	return false
}
