package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/oklog/ulid/v2"
)

type identityFunc func(context.Context, *http.Request) (application.Principal, error)

func TestApprovalWorkflowUnavailableIsNotReportedAsInternalServerError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/approvals/approval-1", nil)
	writeError(context, application.ErrApprovalWorkflowUnavailable)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "CON_APPROVAL_WORKFLOW_UNAVAILABLE") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func (fn identityFunc) Authenticate(ctx context.Context, request *http.Request) (application.Principal, error) {
	return fn(ctx, request)
}

func TestRequestIDReplacesInvalidClientValue(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", "untrusted request id")
	response := httptest.NewRecorder()

	NewRouter(nil, nil, nil).ServeHTTP(response, request)

	id := response.Header().Get("X-Request-ID")
	if _, err := ulid.ParseStrict(id); err != nil {
		t.Fatalf("generated X-Request-ID = %q, want valid ULID: %v", id, err)
	}
}

func TestRequestIDPreservesValidClientULID(t *testing.T) {
	id := ulid.Make().String()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	request.Header.Set("X-Request-ID", id)
	response := httptest.NewRecorder()

	NewRouter(nil, nil, nil).ServeHTTP(response, request)

	if got := response.Header().Get("X-Request-ID"); got != id {
		t.Fatalf("X-Request-ID = %q, want %q", got, id)
	}
}

func TestReadinessExposesRequiredAuditConfiguration(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	NewRouter(nil, nil, nil).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"audit":"disabled"`) {
		t.Fatalf("readiness body does not expose disabled audit: %s", response.Body.String())
	}
}

func TestAuthenticationFailureAbortsGinHandlerChain(t *testing.T) {
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{}, platform.ErrUnauthenticated
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil)
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}

func TestAuthMeReturnsPlatformAuthorizationSnapshot(t *testing.T) {
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{
			TenantID: "tenant-1", UserID: "user-1", DisplayName: "章六", UserName: "zhangliu", Email: "zhangliu@example.com", Roles: []string{"sales"},
			Permissions:           map[string]bool{"contract.create": true, "contract.read": true},
			AuthorizationRevision: 9,
			CatalogVersion:        "catalog-v1",
			DataScopes: []application.DataScope{
				{RoleCode: "sales", ScopeType: "SELF"},
			},
		}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var body struct {
		Data struct {
			TenantID       string                  `json:"tenant_id"`
			UserID         string                  `json:"user_id"`
			DisplayName    string                  `json:"display_name"`
			UserName       string                  `json:"user_name"`
			Email          string                  `json:"email"`
			Role           map[string]string       `json:"role"`
			Permissions    []string                `json:"permissions"`
			AuthzRevision  uint64                  `json:"authorization_revision"`
			CatalogVersion string                  `json:"catalog_version"`
			DataScopes     []application.DataScope `json:"data_scopes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Data.TenantID != "tenant-1" || body.Data.UserID != "user-1" || body.Data.DisplayName != "章六" || body.Data.UserName != "zhangliu" || body.Data.Email != "zhangliu@example.com" ||
		body.Data.Role["code"] != "sales" || body.Data.AuthzRevision != 9 ||
		body.Data.CatalogVersion != "catalog-v1" || len(body.Data.Permissions) != 2 ||
		len(body.Data.DataScopes) != 1 || body.Data.DataScopes[0].RoleCode != "sales" {
		t.Fatalf("data = %#v", body.Data)
	}
}

func TestInvalidJSONDoesNotReachService(t *testing.T) {
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{}, nil
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/contracts", strings.NewReader(`{"unknown":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
}

// stubVerifier 实现 platform.ClientCredentialsTokenVerifier，用于验证路由层只信任
// 验签返回的租户，不信任请求头。
type stubVerifier struct {
	tenantID string
	err      error
}

func (s stubVerifier) VerifyClientCredentials(_ context.Context, _ string) (platform.ServiceTokenIdentity, error) {
	if s.err != nil {
		return platform.ServiceTokenIdentity{}, s.err
	}
	return platform.ServiceTokenIdentity{TenantID: s.tenantID}, nil
}

func TestDashboardIntegrationRequiresExplicitTenantBoundary(t *testing.T) {
	handler := &Handler{}
	newRouter := func(options DashboardIntegrationOptions) *gin.Engine {
		router := gin.New()
		router.GET(
			"/internal/v1/dashboard",
			handler.authenticateDashboardIntegration(options),
			func(c *gin.Context) {
				writeData(c, http.StatusOK, map[string]string{"tenant_id": principal(c).TenantID})
			},
		)
		return router
	}

	// 1) 未强制 Bearer（Enabled=true 但 RequireBearer=false）必须失败关闭，绝不允许
	//    退化为“无认证 + 请求头租户”的组合。
	disabled := newRouter(DashboardIntegrationOptions{Enabled: true})
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard", nil)
	req.Header.Set("X-DA-Tenant-ID", "attacker-tenant")
	rec := httptest.NewRecorder()
	disabled.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-bearer status = %d, want %d; body = %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	// 2) 强制 Bearer 时，缺少令牌返回 401。
	authd := newRouter(DashboardIntegrationOptions{
		Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{tenantID: "tenant-1"},
	})
	req = httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard", nil)
	req.Header.Set("X-DA-Tenant-ID", "attacker-tenant")
	rec = httptest.NewRecorder()
	authd.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	// 3) 有效 Bearer 时，租户取自已验签令牌（stub 返回 tenant-1），请求头 tenant 被忽略。
	req = httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("X-DA-Tenant-ID", "attacker-tenant")
	rec = httptest.NewRecorder()
	authd.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid bearer status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"tenant_id":"tenant-1"`) {
		t.Fatalf("tenant must come from verified token, body = %s", rec.Body.String())
	}

	// 4) 无效令牌返回 401。
	failing := newRouter(DashboardIntegrationOptions{
		Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{err: platform.ErrInvalidServiceToken},
	})
	req = httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	rec = httptest.NewRecorder()
	failing.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestSettlementCompletedContractsRequiresBearer(t *testing.T) {
	router := NewRouterWithSettlement(nil, nil, nil, &SettlementIntegrationOptions{
		Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{tenantID: "tenant-1"},
	})
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/settlement/completed-contracts", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing settlement bearer status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

type prefixedOIDCIdentity struct {
	identityFunc
	prefix string
}

func (identity prefixedOIDCIdentity) Login(http.ResponseWriter, *http.Request)    {}
func (identity prefixedOIDCIdentity) Callback(http.ResponseWriter, *http.Request) {}
func (identity prefixedOIDCIdentity) Logout(http.ResponseWriter, *http.Request)   {}
func (identity prefixedOIDCIdentity) PublicPath(path string) string {
	return strings.TrimRight(identity.prefix, "/") + "/" + strings.TrimLeft(path, "/")
}

func TestWebHomeRedirectPreservesPortalPrefix(t *testing.T) {
	identity := prefixedOIDCIdentity{
		identityFunc: func(context.Context, *http.Request) (application.Principal, error) {
			return application.Principal{}, platform.ErrUnauthenticated
		},
		prefix: "/contract_management",
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusFound)
	}
	if location := response.Header().Get("Location"); location != "/contract_management/auth/login" {
		t.Fatalf("Location = %q, want %q", location, "/contract_management/auth/login")
	}
}
