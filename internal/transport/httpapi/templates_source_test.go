package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

func TestReplaceTemplateSourceHTTPBoundary(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "https://contracts.example.test/contract_management")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	actor := application.Principal{TenantID: "income-tenant", UserID: "manager", Permissions: map[string]bool{"contract.template.manage": true}}
	repo := &incomeHTTPTemplateRepository{items: map[string]contracttemplate.Template{}}
	service := &application.Service{Templates: repo}
	paths, err := filepath.Glob("../../../收入合同模版/[1-5]、*.docx")
	if err != nil || len(paths) != 5 {
		t.Fatal(paths, err)
	}
	original, err := os.ReadFile(paths[1])
	if err != nil {
		t.Fatal(err)
	}
	item, err := service.CreateTemplate(context.Background(), actor, "existing", filepath.Base(paths[1]), original)
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := os.ReadFile(filepath.Join("../../../收入合同模版/修正版", filepath.Base(paths[1])))
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil }), nil)
	replace := func(content []byte, origin string) *httptest.ResponseRecorder {
		request := incomeUploadRequest(t, "ignored", filepath.Base(paths[1]), content)
		request.Method = http.MethodPut
		request.URL.Path = "/api/v1/contract-templates/" + item.ID + "/source"
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	response := replace(corrected, "https://contracts.example.test")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var envelope struct{ Data contracttemplate.Template }
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.ID != item.ID || envelope.Data.Name != "existing" || len(envelope.Data.Fields) <= len(item.Fields) {
		t.Fatal(envelope.Data)
	}
	if response := replace([]byte("not-docx"), "https://contracts.example.test"); response.Code != 422 {
		t.Fatal(response.Code)
	}
	if response := replace(corrected, "https://attacker.example.test"); response.Code != 403 {
		t.Fatal(response.Code)
	}
	actor.TenantID = "another-tenant"
	if response := replace(corrected, "https://contracts.example.test"); response.Code != 404 {
		t.Fatal(response.Code, response.Body.String())
	}
	actor.TenantID = "income-tenant"
	actor.Permissions = map[string]bool{}
	if response := replace(corrected, "https://contracts.example.test"); response.Code != 403 {
		t.Fatal(response.Code)
	}
}
