package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func TestValidateCompactIDTokenClaimsAcceptsStableIdentityOnly(t *testing.T) {
	identity, err := validateCompactIDTokenClaims(oidcIDTokenClaims{Subject: "identity-1", TenantID: "tenant-1", Nonce: "nonce-1", TokenUse: "id_token"}, "nonce-1", "tenant-1")
	if err != nil {
		t.Fatalf("validateCompactIDTokenClaims() error = %v", err)
	}
	if identity.Subject != "identity-1" || identity.IdentityID != identity.Subject {
		t.Fatalf("identity = %#v", identity)
	}
}

func TestValidateCompactIDTokenClaimsRejectsBoundaryMismatch(t *testing.T) {
	tests := []oidcIDTokenClaims{
		{Subject: "identity-1", IdentityID: "identity-2", TenantID: "tenant-1", Nonce: "nonce-1", TokenUse: "id_token"},
		{Subject: "identity-1", TenantID: "tenant-2", Nonce: "nonce-1", TokenUse: "id_token"},
		{Subject: "identity-1", TenantID: "tenant-1", Nonce: "wrong", TokenUse: "id_token"},
		{Subject: "identity-1", TenantID: "tenant-1", Nonce: "nonce-1", TokenUse: "access_token"},
	}
	for _, claims := range tests {
		if _, err := validateCompactIDTokenClaims(claims, "nonce-1", "tenant-1"); err == nil {
			t.Fatalf("claims %#v accepted", claims)
		}
	}
}

func TestPrincipalFromAuthorizationContextAllowsApplicationEmptyScopeID(t *testing.T) {
	catalog := testCatalog()
	principal, err := principalFromAuthorizationContext(compactIdentity{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1"}, AuthorizationContext{
		Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: "contract_management-prod-web", ApplicationCode: "contract_management", EnvironmentCode: "prod",
		Roles: []string{"sales"}, Permissions: []string{"contract.read"}, AuthorizationRevision: 2,
		DataScopes: []AuthorizationDataScope{{RoleCode: "sales", ScopeType: "APPLICATION"}},
	}, catalog, "contract_management-prod-web", "contract_management", "prod")
	if err != nil {
		t.Fatalf("principalFromAuthorizationContext() error = %v", err)
	}
	filter, ok := principal.Scope("contract.read")
	if !ok || !filter.AllowAll {
		t.Fatalf("scope = %#v, %v", filter, ok)
	}
}

func TestPrincipalFromAuthorizationContextTreatsMatchingEnvironmentAsAll(t *testing.T) {
	principal, err := principalFromAuthorizationContext(compactIdentity{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1"}, AuthorizationContext{
		Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: "client-1", ApplicationCode: "contract_management", EnvironmentCode: "prod",
		Roles: []string{"sales"}, Permissions: []string{"contract.read"}, AuthorizationRevision: 1,
		DataScopes: []AuthorizationDataScope{{RoleCode: "sales", ScopeType: "ENVIRONMENT", ScopeID: "env-internal-1", EnvironmentCode: "prod"}},
	}, testCatalog(), "client-1", "contract_management", "prod")
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	filter, ok := principal.Scope("contract.read")
	if !ok || !filter.AllowAll {
		t.Fatalf("scope = %#v", filter)
	}
}

func TestPrincipalFromAuthorizationContextRejectsUnknownAndMalformedScopes(t *testing.T) {
	tests := []AuthorizationDataScope{
		{RoleCode: "sales", ScopeType: "APPLICATION", EnvironmentCode: "prod"},
		{RoleCode: "sales", ScopeType: "ENVIRONMENT", ScopeID: "env-1", EnvironmentCode: "dev"},
		{RoleCode: "sales", ScopeType: "SELF", ScopeID: "other"},
		{RoleCode: "sales", ScopeType: "ORG"},
		{RoleCode: "sales", ScopeType: "UNKNOWN", ScopeID: "x"},
	}
	for _, scope := range tests {
		_, err := principalFromAuthorizationContext(compactIdentity{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1"}, AuthorizationContext{
			Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: "client-1", ApplicationCode: "contract_management", EnvironmentCode: "prod",
			Roles: []string{"sales"}, Permissions: []string{"contract.read"}, AuthorizationRevision: 1, DataScopes: []AuthorizationDataScope{scope},
		}, testCatalog(), "client-1", "contract_management", "prod")
		if err == nil {
			t.Fatalf("scope %#v accepted", scope)
		}
	}
}

func TestPrincipalFromAuthorizationContextRejectsClientApplicationEnvironmentMismatch(t *testing.T) {
	base := AuthorizationContext{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: "wrong", ApplicationCode: "contract_management", EnvironmentCode: "prod", Roles: []string{"sales"}, Permissions: []string{"contract.read"}, AuthorizationRevision: 1, DataScopes: []AuthorizationDataScope{{RoleCode: "sales", ScopeType: "APPLICATION"}}}
	_, err := principalFromAuthorizationContext(compactIdentity{Subject: "identity-1", IdentityID: "identity-1", TenantID: "tenant-1"}, base, testCatalog(), "client-1", "contract_management", "prod")
	if !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestSecretCodecEncryptsAndAuthenticatesValues(t *testing.T) {
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := codec.encrypt("access-token-value")
	if err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == "access-token-value" {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := codec.decrypt(ciphertext)
	if err != nil || got != "access-token-value" {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err := codec.decrypt(ciphertext); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestHTTPAuthorizationContextClientClassifiesStatuses(t *testing.T) {
	for status, expected := range map[int]error{http.StatusUnauthorized: ErrAuthorizationUnauthorized, http.StatusForbidden: ErrAuthorizationForbidden, http.StatusInternalServerError: ErrAuthorizationUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		client, _ := NewHTTPAuthorizationContextClient(server.URL, time.Second, nil)
		_, err := client.Resolve(context.Background(), "token")
		server.Close()
		if !errors.Is(err, expected) {
			t.Fatalf("status %d error = %v", status, err)
		}
	}
}

func TestHTTPAuthorizationContextClientRequiresSingleJSONObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"sub":"one"}{"sub":"two"}`)) }))
	defer server.Close()
	client, _ := NewHTTPAuthorizationContextClient(server.URL, time.Second, nil)
	if _, err := client.Resolve(context.Background(), "token"); !errors.Is(err, ErrAuthorizationInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestSessionPrincipalRoundTripIncludesDataScopes(t *testing.T) {
	want := application.Principal{TenantID: "tenant-1", UserID: "identity-1", IdentityID: "identity-1", AuthorizationRevision: 3, Permissions: map[string]bool{"contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}}}
	raw, _ := json.Marshal(want)
	got, err := principalFromJSON(raw)
	if err != nil || !got.PermissionScopes["contract.read"].AllowAll {
		t.Fatalf("round trip = %#v, %v", got, err)
	}
}

func testCatalog() AuthorizationCatalog {
	return AuthorizationCatalog{Version: "test", Roles: map[string]struct{}{"sales": {}}, Permissions: map[string]struct{}{"contract.read": {}}, RolePermissions: map[string]map[string]struct{}{"sales": {"contract.read": {}}}}
}
