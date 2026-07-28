package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

func TestOIDCLocalSessionUsesIndependentCookie(t *testing.T) {
	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	authenticator := &OIDCAuthenticator{
		options: OIDCOptions{
			SessionCookieName: "contract_management_session",
			SessionTTL:        time.Hour,
			PathPrefix:        "/contract_management",
		},
		now:          func() time.Time { return now },
		transactions: make(map[string]loginTransaction),
		sessions: map[string]localSession{
			"local-session": {
				Principal: application.Principal{TenantID: "tenant-1", UserID: "user-1"},
				ExpiresAt: now.Add(time.Hour),
			},
		},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil)
	request.AddCookie(&http.Cookie{Name: "bp_session", Value: "platform-session"})
	if _, err := authenticator.Authenticate(context.Background(), request); err != ErrUnauthenticated {
		t.Fatalf("Authenticate() with platform cookie error = %v, want ErrUnauthenticated", err)
	}
	request.AddCookie(&http.Cookie{Name: "contract_management_session", Value: "local-session"})
	principal, err := authenticator.Authenticate(context.Background(), request)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if principal.TenantID != "tenant-1" || principal.UserID != "user-1" {
		t.Fatalf("principal = %#v", principal)
	}
}

func TestOIDCSessionCookieIsScopedToSubsystem(t *testing.T) {
	authenticator := &OIDCAuthenticator{options: OIDCOptions{
		SessionCookieName: "contract_management_session", PathPrefix: "/contract_management",
		SessionSecure: true,
	}}
	cookie := authenticator.sessionCookie("session", time.Now().Add(time.Hour))
	if cookie.Path != "/contract_management" || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie = %#v", cookie)
	}
}

func TestOIDCBackchannelTransportRewritesOnlyIssuerOrigin(t *testing.T) {
	publicURL, _ := url.Parse("http://localhost:8081")
	backchannelURL, _ := url.Parse("http://api:8080")
	transport := &backchannelTransport{
		public: publicURL, backchannel: backchannelURL,
		base: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "http://api:8080/oauth2/jwks" {
				t.Fatalf("rewritten URL = %q", request.URL.String())
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
		}),
	}
	request := httptest.NewRequest(http.MethodGet, "http://localhost:8081/oauth2/jwks", nil)
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if request.URL.String() != "http://localhost:8081/oauth2/jwks" {
		t.Fatalf("original request was mutated: %q", request.URL.String())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestOIDCPublicPathUsesConfiguredPortalPrefix(t *testing.T) {
	authenticator := &OIDCAuthenticator{options: OIDCOptions{PathPrefix: "/contract_management"}}
	if got := authenticator.PublicPath("/auth/login"); got != "/contract_management/auth/login" {
		t.Fatalf("PublicPath() = %q, want %q", got, "/contract_management/auth/login")
	}
}
