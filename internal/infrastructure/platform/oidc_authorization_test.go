package platform

import (
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestAuthorizationURLRoutesDirectlyToConfiguredBroker(t *testing.T) {
	authenticator := &OIDCAuthenticator{
		options: OIDCOptions{IdentityProviderHint: "basic-platform"},
		oauth2Config: oauth2.Config{
			ClientID:    "contract_management-prod-web",
			Endpoint:    oauth2.Endpoint{AuthURL: "http://keycloak/realms/basic-platform/protocol/openid-connect/auth"},
			RedirectURL: "http://platform/contract_management/auth/callback",
			Scopes:      []string{"openid", "profile"},
		},
	}
	target, err := url.Parse(authenticator.authorizationURL("state", "nonce", "verifier"))
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	query := target.Query()
	if got := query.Get("kc_idp_hint"); got != "basic-platform" {
		t.Fatalf("kc_idp_hint = %q, want basic-platform", got)
	}
	if query.Get("nonce") != "nonce" || query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" {
		t.Fatalf("authorization query = %#v", query)
	}
}
