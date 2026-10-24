package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
)

// 这些测试使用浏览器传输结构验证真实 HTTP 及应用
// 边界。有意不读取相邻检出目录：
// 独立检出的后端也必须能执行行为测试。
func consistencyActor() application.Principal {
	p := application.Principal{TenantID: "tenant", UserID: "operator", IdentityID: "identity", Roles: []string{"admin"}, Permissions: map[string]bool{}, PermissionScopes: map[string]contract.ScopeFilter{}}
	for _, permission := range []string{"contract.create", "contract.edit", "contract.read", "contract.signing.manage", "approval_rule.manage"} {
		p.Permissions[permission] = true
		p.PermissionScopes[permission] = contract.ScopeFilter{AllowAll: true}
	}
	return p
}

func consistencyRouter(service *application.Service) http.Handler {
	return NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return consistencyActor(), nil
	}), nil)
}

func consistencyMetadata() map[string]any {
	return map[string]any{
		"title": "外部合同", "contract_type": "直签", "service_type": "等保测评",
		"opportunity_id": "9", "opportunity_name": "商机", "crm_customer_id": 7,
		"customer_name": "浏览器客户名称", "customer_address": "北京", "customer_contact": "张三", "customer_phone": "01012345678",
		"amount_minor": 10001, "currency": "CNY", "content": "",
		"payment_terms": []any{map[string]any{"installment_no": 1, "due_date": "2028-02-29", "amount": "100.01"}},
		"start_date":    "2026-10-01T00:00:00Z", "end_date": "2028-03-01T00:00:00Z",
		"service_items": []any{map[string]any{
			"service_type": "等保测评", "name": "服务一", "site": "北京", "batch": "第一批次", "category": "等保测评", "requirement": "三级", "test_mode": "STANDARD",
			"systems": []any{map[string]any{"name": "系统一", "level": "三级"}},
		}},
	}
}

func consistencyJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFrontendContractConsistencyExternalUploadPreservesDocumentAndSchedule(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	for _, test := range []struct {
		name    string
		content *string
	}{
		{name: "current frontend omits text content"},
		{name: "legacy frontend sends empty content", content: stringPointer("")},
		{name: "legacy content cannot replace DOCX", content: stringPointer("client supplied text must not replace uploaded DOCX")},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &externalContractRepository{}
			svc := &application.Service{Repo: repo, DetectionCategories: externalDetectionCategoryDirectory{}, CRMContractReferences: externalCRMReferenceDirectory{}}
			metadata := consistencyMetadata()
			delete(metadata, "content")
			if test.content != nil {
				metadata["content"] = *test.content
			}
			document := externalContractDOCX(t)
			request := externalContractUploadRequest(t, consistencyJSON(t, metadata), "external.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", document)
			response := httptest.NewRecorder()
			consistencyRouter(svc).ServeHTTP(response, request)
			if response.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			got := repo.created
			if got.ID == "" || got.TenantID != "tenant" || got.Content != "外部合同正文" || !bytes.Equal(got.Document, document) || got.TemplateID != "" {
				t.Fatalf("uploaded source was not authoritative: %+v", got)
			}
			if got.CustomerName != "CRM 权威客户" || got.OpportunityName != "CRM 权威商机" || len(got.ServiceItems) != 1 || got.ServiceItems[0].TestMode != "STANDARD" || len(got.Systems) != 1 {
				t.Fatalf("CRM/service metadata lost: %+v", got)
			}
			if len(got.PaymentTerms) != 1 || got.PaymentTerms[0].Amount != "100.01" || got.AmountMinor != 10001 || got.StartDate == nil || got.EndDate == nil {
				t.Fatalf("financial/date metadata lost: %+v", got)
			}
			var result struct{ Data contract.Contract }
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Data.ID != got.ID || result.Data.Content != got.Content || len(result.Data.PaymentTerms) != 1 || result.Data.PaymentTerms[0] != got.PaymentTerms[0] {
				t.Fatal("response disagrees with saved contract")
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

func TestFrontendContractConsistencyExternalMetadataRejectsMalformedWireValues(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	cases := []struct {
		name string
		edit func(map[string]any)
	}{
		{"unknown field", func(p map[string]any) { p["unreviewed_field"] = "x" }},
		{"date-only instead of RFC3339", func(p map[string]any) { p["start_date"] = "2026-10-15" }},
		{"fractional minor units", func(p map[string]any) { p["amount_minor"] = 100.01 }},
		{"string customer ID", func(p map[string]any) { p["crm_customer_id"] = "7" }},
		{"currency is not a letter code without installments", func(p map[string]any) { p["currency"] = "123"; p["payment_terms"] = []any{} }},
		{"reversed contract dates", func(p map[string]any) { p["start_date"] = "2029-01-01T00:00:00Z" }},
		{"localized test mode label", func(p map[string]any) { p["service_items"].([]any)[0].(map[string]any)["test_mode"] = "常规检测" }},
		{"invalid installment date", func(p map[string]any) { p["payment_terms"].([]any)[0].(map[string]any)["due_date"] = "2027-02-29" }},
		{"unbalanced schedule", func(p map[string]any) { p["payment_terms"].([]any)[0].(map[string]any)["amount"] = "100.00" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			repo := &externalContractRepository{}
			metadata := consistencyMetadata()
			tt.edit(metadata)
			request := externalContractUploadRequest(t, consistencyJSON(t, metadata), "external.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", externalContractDOCX(t))
			response := httptest.NewRecorder()
			consistencyRouter(&application.Service{Repo: repo, DetectionCategories: externalDetectionCategoryDirectory{}, CRMContractReferences: externalCRMReferenceDirectory{}}).ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity || repo.created.ID != "" || !strings.Contains(response.Body.String(), `"code":"CON_VALIDATION_ERROR"`) {
				t.Fatalf("invalid metadata reached persistence: status=%d saved=%s body=%s", response.Code, repo.created.ID, response.Body.String())
			}
		})
	}
}

func TestFrontendContractConsistencyTemplateCreateBindsSharedPayload(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	repo := &externalContractRepository{}
	metadata := consistencyMetadata()
	metadata["template_id"] = "template"
	metadata["template_values"] = map[string]string{}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/contracts", strings.NewReader(consistencyJSON(t, metadata)))
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	service := &application.Service{Repo: repo, CRMContractReferences: externalCRMReferenceDirectory{}, Templates: draftHTTPTemplates{item: contracttemplate.Template{ID: "template", TenantID: "tenant", Content: externalContractDOCX(t)}}}
	consistencyRouter(service).ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if repo.created.TemplateID != "template" || repo.created.Content != "外部合同正文" || len(repo.created.PaymentTerms) != 1 || repo.created.PaymentTerms[0].Amount != "100.01" {
		t.Fatalf("template payload lost fields: %+v", repo.created)
	}
}

func TestFrontendContractConsistencyExternalDraftScheduleOmittedOrCleared(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	for _, clearSchedule := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted preserves", true: "empty clears"}[clearSchedule], func(t *testing.T) {
			metadata := consistencyMetadata()
			metadata["expected_version"] = 3
			if clearSchedule {
				metadata["payment_terms"] = []any{}
			} else {
				delete(metadata, "payment_terms")
			}
			document := externalContractDOCX(t)
			repo := &draftHTTPRepository{current: contract.Contract{ID: "draft", TenantID: "tenant", Status: contract.StatusDraft, Version: 3, CreatedBy: "operator", AmountMinor: 10001, Currency: "CNY", Content: "外部合同正文", Document: document, PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2028-02-29", Amount: "100.01"}}}}
			request := httptest.NewRequest(http.MethodPut, "/api/v1/contracts/draft/draft", strings.NewReader(consistencyJSON(t, metadata)))
			request.Header.Set("Origin", "http://example.com")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			consistencyRouter(&application.Service{Repo: repo, DetectionCategories: externalDetectionCategoryDirectory{}, CRMContractReferences: externalCRMReferenceDirectory{}}).ServeHTTP(response, request)
			if response.Code != http.StatusOK || repo.writes != 1 || repo.current.Version != 4 || !bytes.Equal(repo.current.Document, document) {
				t.Fatalf("status=%d writes=%d body=%s", response.Code, repo.writes, response.Body.String())
			}
			if clearSchedule && len(repo.current.PaymentTerms) != 0 || !clearSchedule && len(repo.current.PaymentTerms) != 1 {
				t.Fatalf("nil/empty schedule semantics changed: %+v", repo.current.PaymentTerms)
			}
		})
	}
}

type consistencyRulesRepository struct {
	application.Repository
	saved approval.Rule
}

func (r *consistencyRulesRepository) CreateRule(_ context.Context, value approval.Rule, _ string) error {
	r.saved = value
	return nil
}

func (r *consistencyRulesRepository) UpdateRule(_ context.Context, value approval.Rule, _ string) error {
	r.saved = value
	return nil
}

func TestFrontendContractConsistencyRulesPreserveGroupsAndMinorUnitComparisons(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			repo := &consistencyRulesRepository{}
			payload := `{"name":"分组规则","priority":1,"enabled":true,"version":2,"expression":{"logical":"and","conditions":[{"field":"amount_minor","operator":"gte","value":10001}],"groups":[{"logical":"or","conditions":[{"field":"contract_type","operator":"in","value":["直签","三方"]},{"field":"terms_identical","operator":"eq","value":true}]}]},"nodes":[{"id":"sales","name":"销售审批","role_code":"sales_director","countersign":"any","assignee_ids":[]}]}`
			path := "/api/v1/approval-rules"
			if method == http.MethodPut {
				path += "/rule"
			}
			request := httptest.NewRequest(method, path, strings.NewReader(payload))
			request.Header.Set("Origin", "http://example.com")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			consistencyRouter(&application.Service{Repo: repo}).ServeHTTP(response, request)
			if response.Code != http.StatusCreated && response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if len(repo.saved.Expression.Groups) != 1 || len(repo.saved.Expression.Conditions) != 1 {
				t.Fatalf("rule groups/conditions lost: %+v", repo.saved.Expression)
			}
			var returned struct{ Data approval.Rule }
			if err := json.Unmarshal(response.Body.Bytes(), &returned); err != nil {
				t.Fatal(err)
			}
			for _, facts := range []struct {
				amount      int64
				kind        string
				terms, want bool
			}{{10001, "直签", false, true}, {10000, "直签", true, false}, {10001, "其他", false, false}, {10001, "其他", true, true}} {
				matched, err := returned.Data.Expression.Match(approval.Facts{AmountMinor: facts.amount, ContractType: facts.kind, TermsIdentical: facts.terms})
				if err != nil || matched != facts.want {
					t.Fatalf("saved/returned expression semantics changed: facts=%+v got=%v err=%v", facts, matched, err)
				}
			}
		})
	}
}

type consistencySigningRepository struct {
	application.Repository
	shipment     *contract.SigningShipment
	confirmation *contract.SigningConfirmation
}

func (*consistencySigningRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return contract.Contract{ID: "signed", TenantID: "tenant", Status: contract.StatusActive}, nil
}

func (*consistencySigningRepository) GetSigningRecord(context.Context, string, string) (contract.SigningRecord, error) {
	return contract.SigningRecord{Status: contract.SigningPendingShipment}, nil
}

func (r *consistencySigningRepository) SaveSigningShipment(_ context.Context, _, _, _ string, value contract.SigningShipment) error {
	r.shipment = &value
	return nil
}

func (r *consistencySigningRepository) ConfirmSigning(_ context.Context, _, _, _ string, value contract.SigningConfirmation) error {
	r.confirmation = &value
	return nil
}

func TestFrontendContractConsistencySigningUsesDateOnlyAndBooleanWireTypes(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	cases := []struct {
		name, method, action, payload string
		want                          int
	}{
		{"shipment", "PUT", "shipment", `{"courier_number":"SF123","recipient_name":"张三","recipient_phone":"13812345678","recipient_address":"北京","mailed_at":"2026-10-15"}`, 200},
		{"shipment RFC3339 rejected", "PUT", "shipment", `{"courier_number":"SF123","recipient_name":"张三","recipient_phone":"13812345678","recipient_address":"北京","mailed_at":"2026-10-15T00:00:00Z"}`, 422},
		{"confirmation", "POST", "confirm", `{"seal_verified":true,"signature_verified":true,"signed_at":"2026-10-15","document_version":3,"content_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, 200},
		{"confirmation without document binding", "POST", "confirm", `{"seal_verified":true,"signature_verified":true,"signed_at":"2026-10-15"}`, 422},
		{"confirmation string bool rejected", "POST", "confirm", `{"seal_verified":"true","signature_verified":true,"signed_at":"2026-10-15","document_version":3,"content_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, 422},
		{"unverified confirmation rejected", "POST", "confirm", `{"seal_verified":false,"signature_verified":true,"signed_at":"2026-10-15","document_version":3,"content_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, 422},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			repo := &consistencySigningRepository{}
			request := httptest.NewRequest(tt.method, "/api/v1/signing-records/signed/"+tt.action, strings.NewReader(tt.payload))
			request.Header.Set("Origin", "http://example.com")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			consistencyRouter(&application.Service{Repo: repo}).ServeHTTP(response, request)
			if response.Code != tt.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tt.want == 200 {
				expected := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
				if tt.action == "shipment" && (repo.shipment == nil || !repo.shipment.MailedAt.Equal(expected)) || tt.action == "confirm" && (repo.confirmation == nil || !repo.confirmation.SignedAt.Equal(expected)) {
					t.Fatal("date-only value was not bound to UTC calendar date")
				}
			} else if repo.shipment != nil || repo.confirmation != nil {
				t.Fatal("invalid wire values reached persistence")
			}
		})
	}
}
