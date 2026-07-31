package application

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

type memoryTemplateRepository struct {
	items map[string]contracttemplate.Template
}

func (r *memoryTemplateRepository) CreateTemplate(_ context.Context, item contracttemplate.Template) error {
	if r.items == nil {
		r.items = map[string]contracttemplate.Template{}
	}
	r.items[item.ID] = item
	return nil
}

func (r *memoryTemplateRepository) ListTemplates(_ context.Context, tenantID string) ([]contracttemplate.Template, error) {
	result := []contracttemplate.Template{}
	for _, item := range r.items {
		if item.TenantID == tenantID {
			result = append(result, item)
		}
	}
	return result, nil
}

func (r *memoryTemplateRepository) GetTemplate(_ context.Context, tenantID, id string) (contracttemplate.Template, error) {
	item := r.items[id]
	if item.TenantID != tenantID {
		return contracttemplate.Template{}, errors.New("not found")
	}
	return item, nil
}

func TestCreateTemplateRequiresAdminRole(t *testing.T) {
	service := &Service{Templates: &memoryTemplateRepository{}}
	content := applicationTestDOCX(t, "{{customer_name}}")

	_, err := service.CreateTemplate(context.Background(), Principal{
		TenantID: "tenant-1", UserID: "sales-1", Roles: []string{"sales"},
	}, "标准合同", "standard.docx", content)

	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateTemplate() error = %v, want ErrForbidden", err)
	}
}

func TestCreateTemplateAllowsAdminWithoutPermissions(t *testing.T) {
	templates := &memoryTemplateRepository{}
	service := &Service{Templates: templates}

	created, err := service.CreateTemplate(context.Background(), Principal{
		TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"},
		Permissions: map[string]bool{},
	}, "标准合同", "standard.docx", applicationTestDOCX(t, "{{customer_name}}"))

	if err != nil {
		t.Fatalf("CreateTemplate() error = %v", err)
	}
	if created.Name != "标准合同" || templates.items[created.ID].CreatedBy != "admin-1" {
		t.Fatalf("created = %#v, stored = %#v", created, templates.items[created.ID])
	}
}

func TestListTemplatesAllowsAdminWithoutPermissions(t *testing.T) {
	templates := &memoryTemplateRepository{items: map[string]contracttemplate.Template{
		"template-1": {ID: "template-1", TenantID: "tenant-1", Name: "标准合同"},
	}}
	service := &Service{Templates: templates}

	items, err := service.ListTemplates(context.Background(), Principal{
		TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"},
		Permissions: map[string]bool{},
	})

	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "template-1" {
		t.Fatalf("items = %#v", items)
	}
}

func TestCreateContractRendersAndFreezesTemplateDocument(t *testing.T) {
	templateRepository := &memoryTemplateRepository{items: map[string]contracttemplate.Template{
		"template-1": {
			ID: "template-1", TenantID: "tenant-1", Name: "标准合同",
			Fields:  []contracttemplate.Field{{Name: "customer_name", Label: "Customer Name"}},
			Content: applicationTestDOCX(t, "客户：{{customer_name}}"),
		},
	}}
	contracts := &recordingRepository{}
	service := &Service{Repo: contracts, Templates: templateRepository}
	actor := Principal{
		TenantID: "tenant-1", UserID: "sales-1", DisplayName: "销售一号", Roles: []string{"sales"},
		Permissions: map[string]bool{"contract.create": true},
	}

	created, err := service.CreateContract(context.Background(), actor, contract.Contract{
		Number: "CON-100", Title: "标准合同", Type: "service", ServiceType: "consulting",
		TemplateID: "template-1", TemplateValues: map[string]string{"customer_name": "示例公司"},
	})
	if err != nil {
		t.Fatalf("CreateContract() error = %v", err)
	}
	if created.Content != "客户：示例公司" || len(created.Document) == 0 || created.ContentHash == "" {
		t.Fatalf("created = %#v", created)
	}
	if !bytes.Equal(created.Document, contracts.created.Document) {
		t.Fatal("rendered document was not persisted with contract")
	}
}

func applicationTestDOCX(t *testing.T, text string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range map[string]string{
		"[Content_Types].xml": `<Types xmlns="content-types"></Types>`,
		"word/document.xml":   `<w:document xmlns:w="word"><w:body><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:body></w:document>`,
	} {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
