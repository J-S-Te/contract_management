package platform

import (
	"errors"
	"testing"
)

func TestDashboardClientCredentialsClaimsAcceptKeycloakDefaults(t *testing.T) {
	verifier := &keycloakClientCredentialsVerifier{
		clientID: "data_analysis-dev-machine",
		audience: "basic-platform-application",
		tenantID: "tenant-1",
	}
	claims := serviceTokenClaims{
		AuthorizedParty: "data_analysis-dev-machine",
		ClientID:        "data_analysis-dev-machine",
		Type:            "Bearer",
	}
	if err := verifier.validateClaims(claims, []string{"basic-platform-application", "account"}); err != nil {
		t.Fatalf("default Keycloak service-account claims rejected: %v", err)
	}
}

func TestDashboardClientCredentialsClaimsRejectMismatchedOptionalBindings(t *testing.T) {
	verifier := &keycloakClientCredentialsVerifier{
		clientID: "data_analysis-dev-machine",
		audience: "basic-platform-application",
		tenantID: "tenant-1",
	}
	base := serviceTokenClaims{AuthorizedParty: verifier.clientID, ClientID: verifier.clientID, Type: "Bearer"}
	cases := []struct {
		name      string
		claims    serviceTokenClaims
		audiences []string
	}{
		{name: "wrong token use", claims: serviceTokenClaims{AuthorizedParty: base.AuthorizedParty, Type: base.Type, TokenUse: "id_token"}, audiences: []string{verifier.audience}},
		{name: "wrong client id", claims: serviceTokenClaims{AuthorizedParty: base.AuthorizedParty, ClientID: "other", Type: base.Type}, audiences: []string{verifier.audience}},
		{name: "wrong tenant", claims: serviceTokenClaims{AuthorizedParty: base.AuthorizedParty, Type: base.Type, TenantID: "tenant-2"}, audiences: []string{verifier.audience}},
		{name: "wrong audience", claims: base, audiences: []string{"account"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifier.validateClaims(tc.claims, tc.audiences); !errors.Is(err, ErrInvalidServiceToken) {
				t.Fatalf("expected ErrInvalidServiceToken, got %v", err)
			}
		})
	}
}
