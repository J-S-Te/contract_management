package platform

import (
	"errors"
	"testing"
)

// Keycloak 机器令牌的范围断言与平台 application token 同口径：
// 合法声明必须带唯一 scope，缺 scope 的“默认 service-account 声明”不再被接受。
func TestDashboardClientCredentialsClaimsAcceptKeycloakDefaults(t *testing.T) {
	verifier := &keycloakClientCredentialsVerifier{
		clientID:      "data_analysis-dev-machine",
		audience:      "basic-platform-application",
		tenantID:      "tenant-1",
		requiredScope: "dashboard.contract.read",
	}
	claims := serviceTokenClaims{
		AuthorizedParty: "data_analysis-dev-machine",
		ClientID:        "data_analysis-dev-machine",
		Type:            "Bearer",
		Scopes:          serviceTokenScopes{"dashboard.contract.read"},
	}
	if err := verifier.validateClaims(claims, []string{"basic-platform-application", "account"}); err != nil {
		t.Fatalf("default Keycloak service-account claims rejected: %v", err)
	}
}

// 缺少 scope 的默认声明必须被拒绝：只校验 client_id/audience 的旧口径已收紧。
func TestDashboardClientCredentialsClaimsRejectMissingScope(t *testing.T) {
	verifier := &keycloakClientCredentialsVerifier{
		clientID:      "data_analysis-dev-machine",
		audience:      "basic-platform-application",
		tenantID:      "tenant-1",
		requiredScope: "dashboard.contract.read",
	}
	claims := serviceTokenClaims{
		AuthorizedParty: "data_analysis-dev-machine",
		ClientID:        "data_analysis-dev-machine",
		Type:            "Bearer",
	}
	if err := verifier.validateClaims(claims, []string{"basic-platform-application", "account"}); !errors.Is(err, ErrInvalidServiceToken) {
		t.Fatalf("expected ErrInvalidServiceToken, got %v", err)
	}
}

func TestDashboardClientCredentialsClaimsRejectMismatchedOptionalBindings(t *testing.T) {
	verifier := &keycloakClientCredentialsVerifier{
		clientID:      "data_analysis-dev-machine",
		audience:      "basic-platform-application",
		tenantID:      "tenant-1",
		requiredScope: "dashboard.contract.read",
	}
	base := serviceTokenClaims{AuthorizedParty: verifier.clientID, ClientID: verifier.clientID, Type: "Bearer", Scopes: serviceTokenScopes{verifier.requiredScope}}
	cases := []struct {
		name      string
		claims    serviceTokenClaims
		audiences []string
	}{
		{name: "wrong token use", claims: serviceTokenClaims{AuthorizedParty: base.AuthorizedParty, Type: base.Type, TokenUse: "id_token", Scopes: base.Scopes}, audiences: []string{verifier.audience}},
		{name: "wrong client id", claims: serviceTokenClaims{AuthorizedParty: base.AuthorizedParty, ClientID: "other", Type: base.Type, Scopes: base.Scopes}, audiences: []string{verifier.audience}},
		{name: "wrong tenant", claims: serviceTokenClaims{AuthorizedParty: base.AuthorizedParty, Type: base.Type, TenantID: "tenant-2", Scopes: base.Scopes}, audiences: []string{verifier.audience}},
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
