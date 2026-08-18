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

func TestDashboardIntegrationRequiresExplicitTenantBoundary(t *testing.T) {
	handler := &Handler{}
	router := gin.New()
	router.GET(
		"/internal/v1/dashboard",
		handler.authenticateDashboardIntegration(DashboardIntegrationOptions{Enabled: true}),
		func(c *gin.Context) {
			writeData(c, http.StatusOK, map[string]string{"tenant_id": principal(c).TenantID})
		},
	)

	missingTenantRequest := httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard", nil)
	missingTenantResponse := httptest.NewRecorder()
	router.ServeHTTP(missingTenantResponse, missingTenantRequest)
	if missingTenantResponse.Code != http.StatusBadRequest {
		t.Fatalf(
			"missing tenant status = %d, want %d; body = %s",
			missingTenantResponse.Code,
			http.StatusBadRequest,
			missingTenantResponse.Body.String(),
		)
	}
	if !strings.Contains(missingTenantResponse.Body.String(), "CON_DASHBOARD_TENANT_REQUIRED") {
		t.Fatalf("missing tenant response = %s", missingTenantResponse.Body.String())
	}

	tenantRequest := httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard", nil)
	tenantRequest.Header.Set("X-DA-Tenant-ID", " tenant-1 ")
	tenantResponse := httptest.NewRecorder()
	router.ServeHTTP(tenantResponse, tenantRequest)
	if tenantResponse.Code != http.StatusOK {
		t.Fatalf("tenant status = %d, want %d; body = %s", tenantResponse.Code, http.StatusOK, tenantResponse.Body.String())
	}
	if !strings.Contains(tenantResponse.Body.String(), `"tenant_id":"tenant-1"`) {
		t.Fatalf("tenant response = %s", tenantResponse.Body.String())
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
