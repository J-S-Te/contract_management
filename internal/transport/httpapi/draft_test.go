package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

type draftHTTPRepository struct {
	application.Repository
	current contract.Contract
	items   []contract.Contract
	writes  int
}

func (r *draftHTTPRepository) ListContracts(context.Context, string, string, string, string, int) ([]contract.Contract, error) {
	return r.items, nil
}
func (r *draftHTTPRepository) UpdateContractDraft(_ context.Context, c contract.Contract, version uint64, _ string) error {
	r.writes++
	c.Version = version + 1
	r.current = c
	return nil
}

type draftHTTPTemplates struct {
	application.TemplateRepository
	item contracttemplate.Template
}

func (r draftHTTPTemplates) GetTemplate(context.Context, string, string) (contracttemplate.Template, error) {
	return r.item, nil
}

func draftHTTPActor(role string) application.Principal {
	return application.Principal{TenantID: "tenant", UserID: "user", IdentityID: "identity", Roles: []string{role}, Permissions: map[string]bool{"contract.edit": true, "contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.edit": {AllowAll: true}, "contract.read": {AllowAll: true}}}
}

func TestDraftUpdateHTTPSuccessDecodesFullPayload(t *testing.T) {
	r := &draftHTTPRepository{current: contract.Contract{ID: "C", TenantID: "tenant", TemplateID: "template", CreatedBy: "user", OwnerUserID: "user", OwnerIdentityID: "identity", Status: contract.StatusDraft, Version: 7}}
	s := &application.Service{Repo: r, Templates: draftHTTPTemplates{item: contracttemplate.Template{ID: "template", TenantID: "tenant", Content: externalContractDOCX(t)}}}
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return draftHTTPActor("sales"), nil
	})
	body := `{"expected_version":7,"title":"edited contract","contract_type":"直签","service_type":"软件测试","opportunity_id":"9","opportunity_name":"商机","crm_customer_id":1,"customer_name":"客户","customer_address":"地址","customer_contact":"联系人","customer_phone":"01000000000","customer_credit_level":"A","systems":[{"name":"测试系统","level":"三级"}],"service_items":[{"name":"service","service_type":"软件测试","site":"上海","batch":"第一批次","category":"软件测试","test_mode":"STANDARD","requirement":"能力","systems":[{"name":"测试系统","level":"三级"}]}],"amount_minor":1000000,"currency":"CNY","content":"not authoritative","template_id":"template","template_values":{},"start_date":"2026-10-08T00:00:00Z","end_date":"2026-12-31T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/contracts/C/draft", strings.NewReader(body))
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewRouter(s, identity, nil).ServeHTTP(response, req)
	if response.Code != 200 {
		t.Fatalf("status %d body %s", response.Code, response.Body.String())
	}
	var result struct {
		Data contract.Contract `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	got := result.Data
	if r.writes != 1 || got.Version != 8 || !got.CanEditDraft || got.Title != "edited contract" || got.CustomerContact != "联系人" || got.CustomerPhone != "01000000000" || got.OpportunityID != "9" || got.CustomerCreditLevel != "A" || got.AmountMinor != 1000000 || got.TemplateID != "template" || got.Content == "not authoritative" || got.ContentHash == "" || len(r.current.Document) == 0 {
		t.Fatalf("full update was not persisted/returned: %+v", got)
	}
	if got.StartDate == nil || got.EndDate == nil || got.StartDate.Year() != 2026 || got.EndDate.Month() != time.December || len(got.ServiceItems) != 1 || got.ServiceItems[0].Site != "上海" || got.ServiceItems[0].Requirement != "能力" || len(got.Systems) != 1 {
		t.Fatal("nested payload or date binding failed")
	}
}

func TestDraftHTTPReadCapabilitiesAndTenantBoundary(t *testing.T) {
	own := contract.Contract{ID: "own", TenantID: "tenant", CreatedBy: "user", OwnerUserID: "user", OwnerIdentityID: "identity", Status: contract.StatusDraft, Version: 1}
	other := own
	other.ID = "other"
	other.CreatedBy = "other-user"
	other.OwnerIdentityID = "other-identity"
	assigned := own
	assigned.ID = "assigned"
	assigned.CreatedBy = "another-creator"
	cross := own
	cross.ID = "cross"
	cross.TenantID = "other-tenant"
	for _, role := range []string{"sales", "admin"} {
		t.Run(role, func(t *testing.T) {
			r := &draftHTTPRepository{current: own, items: []contract.Contract{own, other, assigned, cross}}
			identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return draftHTTPActor(role), nil })
			router := NewRouter(&application.Service{Repo: r}, identity, nil)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil))
			if response.Code != 200 {
				t.Fatalf("list status %d", response.Code)
			}
			var result struct {
				Data []contract.Contract `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := 2
			if role == "admin" {
				want = 3
			}
			if len(result.Data) != want {
				t.Fatalf("%s visible count %d", role, len(result.Data))
			}
			for _, item := range result.Data {
				canEdit := role == "admin" || item.ID == "own"
				if item.CanEditDraft != canEdit || item.TenantID != "tenant" {
					t.Fatalf("capability/scope mismatch %+v", item)
				}
			}
			for _, item := range []contract.Contract{own, other, assigned, cross} {
				r.current = item
				response = httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contracts/"+item.ID, nil))
				allowed := item.TenantID == "tenant" && (role == "admin" || item.OwnerIdentityID == "identity")
				if allowed {
					var detail struct {
						Data contract.Contract `json:"data"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
						t.Fatal(err)
					}
					canEdit := role == "admin" || item.ID == "own"
					if response.Code != 200 || detail.Data.CanEditDraft != canEdit {
						t.Fatalf("allowed detail denied %d", response.Code)
					}
				} else if response.Code != 403 {
					t.Fatalf("foreign detail status %d", response.Code)
				}
			}
		})
	}
}

func TestDraftHTTPRejectsCrossTenantAndCrossOriginWrites(t *testing.T) {
	for _, tt := range []struct{ name, tenant, origin string }{{"cross tenant", "other-tenant", "http://example.com"}, {"cross origin", "tenant", "https://attacker.example"}} {
		t.Run(tt.name, func(t *testing.T) {
			r := &draftHTTPRepository{current: contract.Contract{ID: "C", TenantID: tt.tenant, CreatedBy: "user", OwnerUserID: "user", Status: contract.StatusDraft, Version: 1}}
			identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
				return draftHTTPActor("admin"), nil
			})
			req := httptest.NewRequest(http.MethodPut, "/api/v1/contracts/C/draft", strings.NewReader(`{"expected_version":1}`))
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			NewRouter(&application.Service{Repo: r}, identity, nil).ServeHTTP(response, req)
			if response.Code != 403 || r.writes != 0 {
				t.Fatalf("write boundary status %d writes %d", response.Code, r.writes)
			}
		})
	}
}

func (r *draftHTTPRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return r.current, nil
}

func TestDraftUpdateHTTPValidationAndPermission(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		authorized bool
		status     int
	}{
		{"missing version", `{"title":"edited"}`, true, 422},
		{"immutable owner rejected", `{"expected_version":1,"owner_user_id":"other"}`, true, 422},
		{"unauthorized", `{"expected_version":1}`, false, 403},
		{"stale", `{"expected_version":2}`, true, 409},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := &draftHTTPRepository{current: contract.Contract{ID: "C", TenantID: "tenant", CreatedBy: "user", OwnerUserID: "user", Status: contract.StatusDraft, Version: 1}}
			identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
				a := application.Principal{TenantID: "tenant", UserID: "user", Roles: []string{"sales"}}
				if tt.authorized {
					a.Permissions = map[string]bool{"contract.edit": true}
					a.PermissionScopes = map[string]contract.ScopeFilter{"contract.edit": {AllowAll: true}}
				}
				return a, nil
			})
			req := httptest.NewRequest(http.MethodPut, "/api/v1/contracts/C/draft", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Origin", "http://example.com")
			response := httptest.NewRecorder()
			NewRouter(&application.Service{Repo: r}, identity, nil).ServeHTTP(response, req)
			if response.Code != tt.status {
				t.Fatalf("status %d body %s", response.Code, response.Body.String())
			}
		})
	}
}
