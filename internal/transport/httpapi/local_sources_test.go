package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testfixture"
)

type localHTTPReferenceFixture struct {
	application.LocalCatalogRepository
}

// GetLocalCounterparty 提供受控独立目录边界，不请求CRM。
// 参数：ctx为请求，filter为真实租户范围，id为本地引用。
// 返回值：当前租户的测试权威名称和版本。
func (localHTTPReferenceFixture) GetLocalCounterparty(ctx context.Context, filter contract.ScopeFilter, id string) (contract.LocalCounterparty, error) {
	return contract.LocalCounterparty{ID: id, TenantID: filter.TenantID, Name: "本地权威交易对方", Enabled: true, Version: 2}, nil
}

// GetLocalCategory 提供独立本地类别版本，不请求项目系统。
// 参数：ctx为请求，filter为真实范围，id为独立类别引用。
// 返回值：当前租户的启用类别快照。
func (localHTTPReferenceFixture) GetLocalCategory(ctx context.Context, filter contract.ScopeFilter, id string) (contract.LocalCategory, error) {
	return contract.LocalCategory{ID: id, TenantID: filter.TenantID, Name: "软件测试", Code: "SOFTWARE", Enabled: true, Version: 3}, nil
}

// TestLocalPDFHTTPUsesIndependentIdentity 验证真实HTTP/Service链的PDF、付款计划和独立引用。
// 参数：t为测试上下文，持久化边界使用既有隔离内存替身。
// 返回值：无；不冒充真实数据库或浏览器验收。
func TestLocalPDFHTTPUsesIndependentIdentity(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	repo := &externalContractRepository{}
	service := &application.Service{Repo: repo, ExternalPDFEnabled: true, ContractSources: &application.ContractSourceOptions{CounterpartySource: "local", CategorySource: "local", Catalog: localHTTPReferenceFixture{}}}
	metadata := consistencyMetadata()
	delete(metadata, "content")
	metadata["crm_customer_id"] = 0
	metadata["opportunity_id"] = ""
	metadata["opportunity_name"] = ""
	metadata["counterparty_source"] = "LOCAL"
	metadata["local_counterparty_id"] = "01J00000000000000000000001"
	metadata["counterparty_version"] = 2
	metadata["category_source"] = "LOCAL"
	items := metadata["service_items"].([]any)
	item := items[0].(map[string]any)
	item["category_id"] = "01J00000000000000000000002"
	item["category_version"] = 3
	request := externalContractUploadRequest(t, consistencyJSON(t, metadata), "local.pdf", "application/pdf", testfixture.PDF("local HTTP contract"))
	recorder := httptest.NewRecorder()
	consistencyRouter(service).ServeHTTP(recorder, request)
	if recorder.Code != 201 {
		t.Fatalf("LOCAL PDF status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repo.created.CRMCustomerID != 0 || repo.created.LocalCounterpartyID != "01J00000000000000000000001" || repo.created.CustomerName != "本地权威交易对方" || repo.created.IntegrationPolicy != contract.IntegrationNotApplicable || len(repo.created.PaymentTerms) != 1 {
		t.Fatalf("identity/plan lost: %+v", repo.created)
	}
	if strings.Contains(recorder.Body.String(), "integration_policy") {
		t.Fatal("private delivery policy exposed to client")
	}
	for _, body := range []string{`{"name":"名称","tenant_id":"other"}`, `{"name":"名称","phone":"13800000000"}`} {
		request = httptest.NewRequest("POST", "/api/v1/local-catalog/counterparties", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://example.com")
		recorder = httptest.NewRecorder()
		consistencyRouter(service).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnprocessableEntity {
			t.Fatalf("unsafe catalog field accepted: %d %s", recorder.Code, recorder.Body.String())
		}
	}
	request = httptest.NewRequest("POST", "/api/v1/local-catalog/counterparties", strings.NewReader(`{"name":"名称"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.com")
	recorder = httptest.NewRecorder()
	consistencyRouter(service).ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatalf("catalog permission widened: %d %s", recorder.Code, recorder.Body.String())
	}
}
