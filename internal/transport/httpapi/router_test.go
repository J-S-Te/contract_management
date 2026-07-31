package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	NewRouter(nil, nil).ServeHTTP(response, request)

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

	NewRouter(nil, nil).ServeHTTP(response, request)

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

	NewRouter(nil, identity).ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnauthorized, response.Body.String())
	}
}

func TestAuthMeReturnsPlatformAuthorizationSnapshot(t *testing.T) {
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{
			TenantID: "tenant-1", UserID: "user-1", DisplayName: "章六", Roles: []string{"sales"},
			Permissions:    map[string]bool{"contract.create": true, "contract.read": true},
			RoleConfigHash: "hash-1", AuthzRevision: 9,
		}, nil
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	response := httptest.NewRecorder()

	service := &application.Service{UserDisplayNames: map[string]string{"user-2": "蔡总", "user-1": "章六"}}
	NewRouter(service, identity).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var body struct {
		Data struct {
			TenantID       string                      `json:"tenant_id"`
			UserID         string                      `json:"user_id"`
			DisplayName    string                      `json:"display_name"`
			Role           map[string]string           `json:"role"`
			Permissions    []string                    `json:"permissions"`
			RoleConfigHash string                      `json:"role_config_hash"`
			AuthzRevision  uint64                      `json:"authz_revision"`
			UserDirectory  []application.UserReference `json:"user_directory"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Data.TenantID != "tenant-1" || body.Data.UserID != "user-1" || body.Data.DisplayName != "章六" ||
		body.Data.Role["code"] != "sales" || body.Data.AuthzRevision != 9 ||
		body.Data.RoleConfigHash != "hash-1" || len(body.Data.Permissions) != 2 ||
		len(body.Data.UserDirectory) != 2 || body.Data.UserDirectory[0].DisplayName != "章六" ||
		body.Data.UserDirectory[1].DisplayName != "蔡总" {
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

	NewRouter(nil, identity).ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
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

	NewRouter(nil, identity).ServeHTTP(response, request)

	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusFound)
	}
	if location := response.Header().Get("Location"); location != "/contract_management/auth/login" {
		t.Fatalf("Location = %q, want %q", location, "/contract_management/auth/login")
	}
}
