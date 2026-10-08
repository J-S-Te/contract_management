package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/docx"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

type incomeHTTPTemplateRepository struct {
	items map[string]contracttemplate.Template
}

func (r *incomeHTTPTemplateRepository) CreateTemplate(_ context.Context, item contracttemplate.Template) error {
	r.items[item.ID] = item
	return nil
}
func (r *incomeHTTPTemplateRepository) ListTemplates(_ context.Context, tenant string) ([]contracttemplate.Template, error) {
	items := []contracttemplate.Template{}
	for _, item := range r.items {
		if item.TenantID == tenant {
			items = append(items, item)
		}
	}
	return items, nil
}
func (r *incomeHTTPTemplateRepository) GetTemplate(_ context.Context, tenant, id string) (contracttemplate.Template, error) {
	item, exists := r.items[id]
	if !exists || item.TenantID != tenant {
		return contracttemplate.Template{}, apperrors.ErrNotFound
	}
	return item, nil
}
func (r *incomeHTTPTemplateRepository) UpdateTemplate(ctx context.Context, item contracttemplate.Template) error {
	if _, err := r.GetTemplate(ctx, item.TenantID, item.ID); err != nil {
		return err
	}
	r.items[item.ID] = item
	return nil
}
func (r *incomeHTTPTemplateRepository) DeleteTemplate(ctx context.Context, tenant, id string) error {
	if _, err := r.GetTemplate(ctx, tenant, id); err != nil {
		return err
	}
	delete(r.items, id)
	return nil
}

func incomeUploadRequest(t *testing.T, name, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", name); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("tenant_id", "untrusted-upload-tenant"); err != nil {
		t.Fatal(err)
	}
	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/contract-templates", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Origin", "https://contracts.example.test")
	t.Cleanup(func() {
		if request.MultipartForm != nil {
			if err := request.MultipartForm.RemoveAll(); err != nil {
				t.Errorf("cleanup multipart files: %v", err)
			}
		}
	})
	return request
}

func TestIncomeTemplatesHTTPRealUploadsAndTenantIsolation(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "https://contracts.example.test/contract_management")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	paths, err := filepath.Glob("../../../收入合同模版/[1-5]、*.docx")
	if err != nil || len(paths) != 5 {
		t.Fatalf("templates=%v error=%v want five documents", paths, err)
	}
	repository := &incomeHTTPTemplateRepository{items: map[string]contracttemplate.Template{}}
	actor := application.Principal{TenantID: "income-tenant", UserID: "manager", Permissions: map[string]bool{"contract.template.manage": true, "contract.create": true}}
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil })
	router := NewRouter(&application.Service{Templates: repository}, identity, nil)
	var firstID string
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			name := strings.TrimSuffix(filepath.Base(path), ".docx")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, incomeUploadRequest(t, name, filepath.Base(path), content))
			if response.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var body struct {
				Data contracttemplate.Template `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			stored, exists := repository.items[body.Data.ID]
			if !exists || body.Data.Name != name || stored.TenantID != actor.TenantID || stored.CreatedBy != actor.UserID || !bytes.Equal(stored.Content, content) || len(body.Data.Fields) == 0 {
				t.Fatalf("invalid imported template: %+v", body.Data)
			}
			if firstID == "" {
				firstID = body.Data.ID
			}
			values := make(map[string]string, len(body.Data.Fields))
			for _, field := range body.Data.Fields {
				values[field.Name] = "回归测试" + field.Name
			}
			values["合同金额"] = "123456.78"
			payload, err := json.Marshal(map[string]any{"values": values})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/contract-templates/"+body.Data.ID+"/preview", bytes.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Origin", "https://contracts.example.test")
			preview := httptest.NewRecorder()
			router.ServeHTTP(preview, request)
			var previewBody struct {
				Data struct {
					HTML string `json:"html"`
				} `json:"data"`
			}
			if err := json.Unmarshal(preview.Body.Bytes(), &previewBody); err != nil {
				t.Fatal(err)
			}
			if preview.Code != http.StatusOK || !strings.Contains(previewBody.Data.HTML, values["客户名称"]) || strings.Contains(previewBody.Data.HTML, "{{") {
				t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
			}
		})
	}
	for _, tenant := range []string{"income-tenant", "another-tenant"} {
		actor.TenantID = tenant
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-templates", nil))
		var body struct {
			Data []contracttemplate.Template `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		want := 5
		if tenant != "income-tenant" {
			want = 0
		}
		if response.Code != http.StatusOK || len(body.Data) != want {
			t.Fatalf("tenant=%s status=%d templates=%d body=%s", tenant, response.Code, len(body.Data), response.Body.String())
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		path := "/api/v1/contract-templates/" + firstID
		if method == http.MethodPost {
			path += "/preview"
		}
		request := httptest.NewRequest(method, path, strings.NewReader(`{"values":{}}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "https://contracts.example.test")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant %s status=%d body=%s", method, response.Code, response.Body.String())
		}
	}
	if len(repository.items) != 5 {
		t.Fatal("cross-tenant request modified templates")
	}
}

func TestIncomeTemplatesHTTPRejectsInvalidUploadsAndAccess(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "https://contracts.example.test")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	paths, err := filepath.Glob("../../../收入合同模版/[1-5]、*.docx")
	if err != nil || len(paths) != 5 {
		t.Fatalf("templates=%v error=%v", paths, err)
	}
	content, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, templateName, filename, origin string
		content                              []byte
		forbidden, unauthenticated           bool
		status                               int
		code                                 string
	}{
		{name: "missing file", templateName: "合同", origin: "https://contracts.example.test", status: 422, code: "CON_VALIDATION_ERROR"},
		{name: "empty file", templateName: "合同", filename: "empty.docx", origin: "https://contracts.example.test", status: 422, code: "CON_VALIDATION_ERROR"},
		{name: "oversized file", templateName: "合同", filename: "oversized.docx", content: make([]byte, docx.MaxTemplateSize+1), origin: "https://contracts.example.test", status: 422, code: "CON_VALIDATION_ERROR"},
		{name: "invalid zip", templateName: "合同", filename: "invalid.docx", content: []byte("not DOCX"), origin: "https://contracts.example.test", status: 422, code: "CON_TEMPLATE_VALIDATION_ERROR"},
		{name: "wrong extension", templateName: "合同", filename: "contract.pdf", content: content, origin: "https://contracts.example.test", status: 422, code: "CON_TEMPLATE_VALIDATION_ERROR"},
		{name: "missing name", filename: "contract.docx", content: content, origin: "https://contracts.example.test", status: 422, code: "CON_TEMPLATE_VALIDATION_ERROR"},
		{name: "no management permission", templateName: "合同", filename: "contract.docx", content: content, origin: "https://contracts.example.test", forbidden: true, status: 403},
		{name: "authentication failure", templateName: "合同", filename: "contract.docx", content: content, origin: "https://contracts.example.test", unauthenticated: true, status: 401},
		{name: "missing origin", templateName: "合同", filename: "contract.docx", content: content, status: 403, code: "AUTH_ORIGIN_REJECTED"},
		{name: "cross origin", templateName: "合同", filename: "contract.docx", content: content, origin: "https://attacker.example.test", status: 403, code: "AUTH_ORIGIN_REJECTED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := &incomeHTTPTemplateRepository{items: map[string]contracttemplate.Template{}}
			identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
				if test.unauthenticated {
					return application.Principal{}, platform.ErrUnauthenticated
				}
				return application.Principal{TenantID: "income-tenant", UserID: "manager", Permissions: map[string]bool{"contract.template.manage": !test.forbidden}}, nil
			})
			request := incomeUploadRequest(t, test.templateName, test.filename, test.content)
			request.Header.Set("Origin", test.origin)
			response := httptest.NewRecorder()
			NewRouter(&application.Service{Templates: repository}, identity, nil).ServeHTTP(response, request)
			if response.Code != test.status || test.code != "" && !strings.Contains(response.Body.String(), test.code) {
				t.Fatalf("status=%d body=%s want status=%d code=%s", response.Code, response.Body.String(), test.status, test.code)
			}
			if len(repository.items) != 0 {
				t.Fatal("rejected request persisted a template")
			}
		})
	}
}
