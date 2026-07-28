package httpapi

import (
	"context"
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
