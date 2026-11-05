package crm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// provisionTestClient 返回绑定固定令牌/时间/nonce 的开立客户端。
// 参数：baseURL 为假服务地址，transport 非空时替换默认 HTTP 传输。
// 返回值：可直接调用的客户端。
func provisionTestClient(baseURL string, transport http.RoundTripper) *ImportProvisionClient {
	client := &ImportProvisionClient{
		BaseURL:     baseURL,
		TokenSource: func(context.Context) (string, error) { return "machine-token", nil },
		Now:         func() time.Time { return time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC) },
		Nonce:       func() (string, error) { return "nonce-1", nil },
	}
	if transport != nil {
		client.Client = &http.Client{Transport: transport}
	}
	return client
}

// TestImportProvisionClientUsesContractHeadersOnAllEndpoints 验证四个端点的路径、
// 重放防护头、幂等键与请求体白名单字段。
// 参数：t 为测试上下文。
// 返回值：无；路径/头/体不符时失败。
func TestImportProvisionClientUsesContractHeadersOnAllEndpoints(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		seen[request.URL.Path] = true
		if request.Method != http.MethodPost {
			t.Errorf("method=%s path=%s", request.Method, request.URL.Path)
		}
		for header, want := range map[string]string{
			"Authorization":           "Bearer machine-token",
			"Accept":                  "application/json",
			"Content-Type":            "application/json",
			"Idempotency-Key":         "attempt-1",
			"X-Actor-Identity-ID":     "identity-1",
			"X-Integration-Timestamp": "2026-11-05T01:02:03Z",
			"X-Integration-Nonce":     "nonce-1",
		} {
			if got := request.Header.Get(header); got != want {
				t.Errorf("path=%s header %s=%q want %q", request.URL.Path, header, got, want)
			}
		}
		body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("path=%s body=%s err=%v", request.URL.Path, body, err)
		}
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/v1/internal/import-provision/customers/lookup":
			if payload["name"] != "客户A" {
				t.Errorf("lookup payload=%v", payload)
			}
			_, _ = io.WriteString(response, `{"code":"OK","data":{"candidates":[{"id":7,"name":"客户A","status":"ACTIVE"}]}}`)
		case "/api/v1/internal/import-provision/customers":
			if payload["owner_identity_id"] != "identity-1" || payload["customer_type"] != "企业" {
				t.Errorf("customer payload=%v", payload)
			}
			_, _ = io.WriteString(response, `{"code":"OK","data":{"customer":{"id":7,"name":"客户A","status":"ACTIVE","origin":"IMPORT_PROVISION"}}}`)
		case "/api/v1/internal/import-provision/opportunities":
			if payload["customer_id"] != float64(7) || payload["expected_sign_date"] != "2020-01-02" {
				t.Errorf("opportunity payload=%v", payload)
			}
			_, _ = io.WriteString(response, `{"code":"OK","data":{"opportunity":{"id":9,"name":"商机A","customer_id":7,"status":"FOLLOWING"}}}`)
		case "/api/v1/internal/import-provision/opportunities/9/sign":
			if payload["contract_ref"] != "CM-2020-001" {
				t.Errorf("sign payload=%v", payload)
			}
			_, _ = io.WriteString(response, `{"code":"OK","data":{"opportunity":{"id":9,"name":"商机A","customer_id":7,"status":"SIGNED","stage":"CLOSED"}}}`)
		default:
			t.Errorf("unexpected path=%s", request.URL.Path)
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := provisionTestClient(server.URL, nil)
	ctx := context.Background()

	candidates, err := client.LookupCustomer(ctx, "identity-1", "attempt-1", "客户A")
	if err != nil || len(candidates) != 1 || candidates[0].ID != 7 || candidates[0].Status != "ACTIVE" {
		t.Fatalf("lookup=%+v err=%v", candidates, err)
	}
	customer, err := client.CreateCustomer(ctx, "identity-1", "attempt-1", ProvisionCustomerInput{
		Name: "客户A", CustomerType: "企业", OwnerIdentityID: "identity-1",
	})
	if err != nil || customer.ID != 7 || customer.Origin != "IMPORT_PROVISION" {
		t.Fatalf("customer=%+v err=%v", customer, err)
	}
	opportunity, err := client.CreateOpportunity(ctx, "identity-1", "attempt-1", ProvisionOpportunityInput{
		CustomerID: 7, OwnerIdentityID: "identity-1", Name: "商机A", Amount: "12500.00", ExpectedSignDate: "2020-01-02",
	})
	if err != nil || opportunity.ID != 9 {
		t.Fatalf("opportunity=%+v err=%v", opportunity, err)
	}
	signed, err := client.SignOpportunity(ctx, "identity-1", "attempt-1", 9, "CM-2020-001")
	if err != nil || signed.Status != "SIGNED" || signed.Stage != "CLOSED" {
		t.Fatalf("signed=%+v err=%v", signed, err)
	}
	for _, path := range []string{
		"/api/v1/internal/import-provision/customers/lookup", "/api/v1/internal/import-provision/customers",
		"/api/v1/internal/import-provision/opportunities", "/api/v1/internal/import-provision/opportunities/9/sign",
	} {
		if !seen[path] {
			t.Fatalf("endpoint %s was not called", path)
		}
	}
}

// TestImportProvisionClientLookupReturnsEmptyCandidates 验证 0 命中不是错误。
// 参数：t 为测试上下文。
// 返回值：无；空候选被当成失败时失败。
func TestImportProvisionClientLookupReturnsEmptyCandidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, `{"code":"OK","message":"success","data":{"candidates":[]}}`)
	}))
	defer server.Close()
	candidates, err := provisionTestClient(server.URL, nil).LookupCustomer(context.Background(), "identity-1", "attempt-1", "客户A")
	if err != nil || len(candidates) != 0 {
		t.Fatalf("lookup=%+v err=%v", candidates, err)
	}
}

// TestImportProvisionClientClassifiesRejectionsAndDependencyFailures 验证错误分类：
// 4xx+code → typed 业务拒绝；5xx/无 code/网络/超限 → 可重试依赖故障。
// 参数：t 为测试上下文。
// 返回值：无；分类不符时失败。
func TestImportProvisionClientClassifiesRejectionsAndDependencyFailures(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		status     int
		body       string
		transport  func(*http.Request) (*http.Response, error)
		wantCode   string
		retryable  bool
		wantSubstr string
	}{
		{name: "customer-ambiguous", status: http.StatusConflict, wantCode: "CRM_IMPORT_PROVISION_CUSTOMER_AMBIGUOUS",
			body: `{"code":"CRM_IMPORT_PROVISION_CUSTOMER_AMBIGUOUS","message":"multiple customers","request_id":"r1"}`},
		{name: "owner-invalid", status: http.StatusUnprocessableEntity, wantCode: "CRM_IMPORT_PROVISION_OWNER_INVALID",
			body: `{"code":"CRM_IMPORT_PROVISION_OWNER_INVALID","message":"owner invalid"}`},
		{name: "server-error", status: http.StatusBadGateway, retryable: true, wantSubstr: "status 502"},
		{name: "client-error-without-code", status: http.StatusBadRequest, retryable: true, wantSubstr: "status 400"},
		{name: "network-failure", retryable: true, wantSubstr: "call CRM import provision",
			transport: func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			transport := testCase.transport
			if transport == nil {
				transport = func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: testCase.status, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(testCase.body))}, nil
				}
			}
			client := provisionTestClient("https://crm.example", roundTripFunc(transport))
			_, err := client.CreateCustomer(context.Background(), "identity-1", "attempt-1", ProvisionCustomerInput{
				Name: "客户A", OwnerIdentityID: "identity-1",
			})
			if err == nil {
				t.Fatal("expected failure")
			}
			if got := ImportProvisionRetryable(err); got != testCase.retryable {
				t.Fatalf("retryable=%v want %v err=%v", got, testCase.retryable, err)
			}
			if testCase.wantCode != "" {
				var rejected *ProvisionRejectedError
				if !errors.As(err, &rejected) || rejected.Code != testCase.wantCode || rejected.StatusCode != testCase.status {
					t.Fatalf("rejected=%+v err=%v", rejected, err)
				}
			}
			if testCase.wantSubstr != "" && !strings.Contains(err.Error(), testCase.wantSubstr) {
				t.Fatalf("error=%v want substring %q", err, testCase.wantSubstr)
			}
		})
	}
}

// TestImportProvisionClientRejectsOversizedResponse 验证响应体超过 1MiB 即失败。
// 参数：t 为测试上下文。
// 返回值：无；超限响应被解析时失败。
func TestImportProvisionClientRejectsOversizedResponse(t *testing.T) {
	oversized := `{"code":"OK","data":{"customer":{"id":7,"name":"` + strings.Repeat("A", 1<<20) + `"}}}`
	client := provisionTestClient("https://crm.example", roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(oversized))}, nil
	}))
	_, err := client.CreateCustomer(context.Background(), "identity-1", "attempt-1", ProvisionCustomerInput{
		Name: "客户A", OwnerIdentityID: "identity-1",
	})
	if !errors.Is(err, ErrImportProvisionUnavailable) || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversized response error=%v", err)
	}
}

// TestImportProvisionClientFailsClosedWithoutRequiredInputs 验证缺少幂等键、操作者
// 或必要业务字段时不会发出请求。
// 参数：t 为测试上下文。
// 返回值：无；发请求或返回 nil 时失败。
func TestImportProvisionClientFailsClosedWithoutRequiredInputs(t *testing.T) {
	requests := 0
	client := provisionTestClient("https://crm.example", roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"data":{"candidates":[]}}`))}, nil
	}))
	ctx := context.Background()
	if _, err := client.LookupCustomer(ctx, "identity-1", "", "客户A"); err == nil {
		t.Fatal("empty idempotency key accepted")
	}
	if _, err := client.LookupCustomer(ctx, "", "attempt-1", "客户A"); err == nil {
		t.Fatal("empty actor accepted")
	}
	if _, err := client.CreateOpportunity(ctx, "identity-1", "attempt-1", ProvisionOpportunityInput{CustomerID: 7, OwnerIdentityID: "identity-1", Name: "商机A"}); err == nil {
		t.Fatal("missing amount accepted")
	}
	if _, err := client.SignOpportunity(ctx, "identity-1", "attempt-1", 0, "CM-1"); err == nil {
		t.Fatal("missing opportunity id accepted")
	}
	if _, err := client.SignOpportunity(ctx, "identity-1", "attempt-1", 9, " "); err == nil {
		t.Fatal("missing contract ref accepted")
	}
	if _, err := (&ImportProvisionClient{}).LookupCustomer(ctx, "identity-1", "attempt-1", "客户A"); err == nil {
		t.Fatal("unconfigured client accepted a call")
	}
	if requests != 0 {
		t.Fatalf("unexpected requests=%d", requests)
	}
}

// TestImportProvisionTokenSourceRequestsSingleDedicatedScope 验证机器令牌只申请一条开立链路的
// 专用 scope，且多 scope 被拒绝在构造期（平台侧会以 invalid_scope 拒绝多值）。
// 参数：t 为测试上下文。
// 返回值：无；scope 或客户端凭据不符时失败。
func TestImportProvisionTokenSourceRequestsSingleDedicatedScope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		clientID, secret, ok := request.BasicAuth()
		if !ok || clientID != "contract-import-provision" || secret != "secret" {
			t.Errorf("token request auth=%q", request.Header.Get("Authorization"))
		}
		if request.FormValue("grant_type") != "client_credentials" ||
			request.FormValue("scope") != "customer.import_provision" {
			t.Errorf("token request form=%v", request.Form)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"token-1","token_type":"Bearer","scope":"customer.import_provision"}`)
	}))
	defer server.Close()
	source := NewImportProvisionTokenSource(context.Background(), server.URL, "contract-import-provision", "secret", "customer.import_provision")
	if source == nil {
		t.Fatal("token source not built for complete credentials")
	}
	token, err := source(context.Background())
	if err != nil || token != "token-1" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if NewImportProvisionTokenSource(context.Background(), server.URL, "contract-import-provision", "secret",
		"customer.import_provision opportunity.import_provision") != nil {
		t.Fatal("multi-scope credentials accepted")
	}
	if NewImportProvisionTokenSource(context.Background(), server.URL, "", "secret", "customer.import_provision") != nil {
		t.Fatal("incomplete credentials accepted")
	}
	if NewImportProvisionClient(context.Background(), "https://crm.example", "", "id", "secret", "customer.import_provision") != nil {
		t.Fatal("client built without token url")
	}
}
