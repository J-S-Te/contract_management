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
			SessionCookieName:            "contract_management_session",
			SessionTTL:                   time.Hour,
			AuthorizationRefreshInterval: time.Minute,
			PathPrefix:                   "/contract_management",
		},
		now:          func() time.Time { return now },
		transactions: make(map[string]loginTransaction),
		sessions: map[string]*localSession{
			"local-session": {
				Principal:      application.Principal{TenantID: "tenant-1", UserID: "user-1"},
				RefreshedAt:    now,
				TokenExpiresAt: now.Add(time.Hour),
				ExpiresAt:      now.Add(time.Hour),
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

func TestOIDCLocalLogoutClearsOnlySubsystemSessionWithoutRedirect(t *testing.T) {
	authenticator := &OIDCAuthenticator{
		options: OIDCOptions{SessionCookieName: "contract_management_session", PathPrefix: "/contract_management"},
		sessions: map[string]*localSession{
			"old-user-session": {IDToken: "old-id-token"},
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/auth/local-logout", nil)
	request.AddCookie(&http.Cookie{Name: "contract_management_session", Value: "old-user-session"})
	response := httptest.NewRecorder()

	authenticator.LogoutLocal(response, request)

	if response.Code != http.StatusNoContent || response.Header().Get("Location") != "" {
		t.Fatalf("status = %d, location = %q", response.Code, response.Header().Get("Location"))
	}
	if _, exists := authenticator.sessions["old-user-session"]; exists {
		t.Fatal("old subsystem session still exists")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "contract_management_session" || cookies[0].MaxAge != -1 || cookies[0].Path != "/contract_management" {
		t.Fatalf("expired cookies = %#v", cookies)
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

func TestPrincipalFromPlatformClaimsUsesPlatformAuthorization(t *testing.T) {
	principal, err := principalFromPlatformClaims(platformIDTokenClaims{
		Subject: "user-1", TenantID: "tenant-1", Roles: []string{"sales"},
		Permissions:    []string{"contract.read", "contract.create"},
		RoleConfigHash: "hash-1", AuthzRevision: 7,
	}, "tenant-1")
	if err != nil {
		t.Fatalf("principalFromPlatformClaims() error = %v", err)
	}
	if principal.UserID != "user-1" || principal.TenantID != "tenant-1" ||
		!principal.Has("contract.read") || principal.Has("approval.process") ||
		principal.AuthzRevision != 7 || principal.RoleConfigHash != "hash-1" {
		t.Fatalf("principal = %#v", principal)
	}
}

func TestPrincipalFromPlatformClaimsRejectsTenantMismatch(t *testing.T) {
	_, err := principalFromPlatformClaims(platformIDTokenClaims{
		Subject: "user-1", TenantID: "tenant-2", Roles: []string{"sales"},
		Permissions: []string{"contract.read"}, RoleConfigHash: "hash-1", AuthzRevision: 1,
	}, "tenant-1")
	if err == nil {
		t.Fatal("principalFromPlatformClaims() error = nil, want tenant mismatch")
	}
}

func TestPrincipalFromPlatformClaimsRejectsWildcardPermission(t *testing.T) {
	_, err := principalFromPlatformClaims(platformIDTokenClaims{
		Subject: "admin-1", TenantID: "tenant-1", Roles: []string{"admin"},
		Permissions: []string{"all"}, RoleConfigHash: "hash-1", AuthzRevision: 2,
	}, "tenant-1")
	if err == nil {
		t.Fatal("principalFromPlatformClaims() error = nil, want wildcard rejection")
	}
}

func TestNormalizePersonnelDirectoryKeepsOnlyUniqueNamedPlatformUsers(t *testing.T) {
	got := normalizePersonnelDirectory([]application.UserReference{
		{UserID: " user-1 ", DisplayName: " 章六 ", Roles: []string{" tech_director ", "sales_director", "tech_director", ""}},
		{UserID: "user-1", DisplayName: "重复姓名"},
		{UserID: "user-2", DisplayName: ""},
		{UserID: "user-3", DisplayName: "蔡总", Roles: []string{"finance_director"}},
	})
	if len(got) != 2 || got[0].UserID != "user-1" || got[0].DisplayName != "章六" ||
		len(got[0].Roles) != 2 || got[0].Roles[0] != "sales_director" || got[0].Roles[1] != "tech_director" ||
		got[1].UserID != "user-3" || got[1].DisplayName != "蔡总" ||
		len(got[1].Roles) != 1 || got[1].Roles[0] != "finance_director" {
		t.Fatalf("normalizePersonnelDirectory() = %#v", got)
	}
}
