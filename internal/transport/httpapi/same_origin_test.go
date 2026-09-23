package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// SEC-D11：/api/v1 写请求必须携带与 APP_PUBLIC_URL 一致的 Origin；
// 缺失、跨源、Sec-Fetch-Site=cross-site、配置缺失（失败关闭）一律被拒。
func TestSameOriginWriteRejectsMissingAndCrossOrigin(t *testing.T) {
	tests := []struct {
		name         string
		appPublicURL string
		extraOrigins string
		origin       string
		secFetchSite string
	}{
		{name: "missing origin", appPublicURL: "http://example.com/contract_management/"},
		{name: "cross origin", appPublicURL: "http://example.com/contract_management/", origin: "https://evil.example"},
		{name: "origin with unexpected host", appPublicURL: "http://example.com/contract_management/", origin: "http://attacker.example"},
		{name: "cross-site fetch metadata", appPublicURL: "http://example.com/contract_management/", origin: "http://example.com", secFetchSite: "cross-site"},
		{name: "empty public url fails closed", appPublicURL: "", origin: "http://example.com"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("APP_PUBLIC_URL", test.appPublicURL)
			t.Setenv("APP_CORS_ALLOWED_ORIGINS", test.extraOrigins)
			gin.SetMode(gin.TestMode)
			router := NewRouter(nil, auditTestIdentity(), nil)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.secFetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.secFetchSite)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "AUTH_ORIGIN_REJECTED") {
				t.Fatalf("status=%d body=%s, want 403 AUTH_ORIGIN_REJECTED", response.Code, response.Body.String())
			}
		})
	}
}

// SEC-D11 正向基线：同源 Origin 可通过校验进入业务链路；
// 附加允许源（APP_CORS_ALLOWED_ORIGINS）同样生效；GET 不受影响。
func TestSameOriginWriteAllowsConfiguredOriginsAndLeavesReadsUntouched(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management/")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "http://localhost:5173, https://platform.example.com")
	gin.SetMode(gin.TestMode)
	router := NewRouter(nil, auditTestIdentity(), nil)

	// 主配置同源：业务链路给出 403 AUTH_FORBIDDEN（无权限），而非来源拒绝。
	primary := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
	primary.Header.Set("Content-Type", "application/json")
	primary.Header.Set("Origin", "http://example.com")
	primaryResponse := httptest.NewRecorder()
	router.ServeHTTP(primaryResponse, primary)
	if primaryResponse.Code != http.StatusForbidden || !strings.Contains(primaryResponse.Body.String(), "AUTH_FORBIDDEN") {
		t.Fatalf("primary origin status=%d body=%s", primaryResponse.Code, primaryResponse.Body.String())
	}

	// 附加允许源同样放行到业务链路。
	extra := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
	extra.Header.Set("Content-Type", "application/json")
	extra.Header.Set("Origin", "http://localhost:5173")
	extraResponse := httptest.NewRecorder()
	router.ServeHTTP(extraResponse, extra)
	if extraResponse.Code != http.StatusForbidden || !strings.Contains(extraResponse.Body.String(), "AUTH_FORBIDDEN") {
		t.Fatalf("extra origin status=%d body=%s", extraResponse.Code, extraResponse.Body.String())
	}

	// 读请求不经过来源校验：403 来自业务权限（列表接口要求 contract.read），
	// 只要不是 AUTH_ORIGIN_REJECTED 即说明读路径未被同源中间件拦截。
	read := httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil)
	readResponse := httptest.NewRecorder()
	router.ServeHTTP(readResponse, read)
	if strings.Contains(readResponse.Body.String(), "AUTH_ORIGIN_REJECTED") {
		t.Fatalf("reads must be exempt from origin checks, status=%d body=%s", readResponse.Code, readResponse.Body.String())
	}
}
