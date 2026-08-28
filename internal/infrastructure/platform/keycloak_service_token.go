package platform

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// KeycloakClientCredentialsVerifierOptions 仅用于尚未迁移的内部接口。
type KeycloakClientCredentialsVerifierOptions struct {
	Issuer, BackchannelBaseURL, ClientID, Audience, TenantID string
	Timeout                                                  time.Duration
}

type keycloakClientCredentialsVerifier struct {
	verifier                     *oidc.IDTokenVerifier
	clientID, audience, tenantID string
}

type serviceTokenClaims struct {
	AuthorizedParty string `json:"azp"`
	ClientID        string `json:"client_id"`
	Type            string `json:"typ"`
	TokenUse        string `json:"token_use"`
	TenantID        string `json:"tenant_id"`
}

// NewKeycloakClientCredentialsTokenVerifier 构建旧 Keycloak 机器域验证器。
// 该构造器只服务尚未迁移的路由，禁止用于新的内部接口。
func NewKeycloakClientCredentialsTokenVerifier(ctx context.Context, options KeycloakClientCredentialsVerifierOptions) (ClientCredentialsTokenVerifier, error) {
	issuer := strings.TrimRight(strings.TrimSpace(options.Issuer), "/")
	if issuer == "" || options.ClientID == "" || options.Audience == "" || options.TenantID == "" {
		return nil, ErrInvalidServiceToken
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: options.Timeout}
	if options.BackchannelBaseURL != "" {
		publicURL, err := url.Parse(issuer)
		if err != nil {
			return nil, err
		}
		backchannelURL, err := url.Parse(strings.TrimRight(options.BackchannelBaseURL, "/"))
		if err != nil {
			return nil, err
		}
		client.Transport = &backchannelTransport{base: http.DefaultTransport, public: publicURL, backchannel: backchannelURL}
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidServiceToken, err)
	}
	return &keycloakClientCredentialsVerifier{verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}), clientID: options.ClientID, audience: options.Audience, tenantID: options.TenantID}, nil
}

func (verifier *keycloakClientCredentialsVerifier) VerifyClientCredentials(ctx context.Context, rawToken string) (ServiceTokenIdentity, error) {
	token, err := verifier.verifier.Verify(ctx, rawToken)
	if err != nil {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	claims := serviceTokenClaims{}
	if err := token.Claims(&claims); err != nil {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	if err := verifier.validateClaims(claims, token.Audience); err != nil {
		return ServiceTokenIdentity{}, err
	}
	return ServiceTokenIdentity{TenantID: verifier.tenantID}, nil
}

func (verifier *keycloakClientCredentialsVerifier) validateClaims(claims serviceTokenClaims, audiences []string) error {
	if !strings.EqualFold(claims.Type, "bearer") || claims.AuthorizedParty != verifier.clientID || !containsAudience(audiences, verifier.audience) ||
		(claims.ClientID != "" && claims.ClientID != verifier.clientID) || (claims.TokenUse != "" && claims.TokenUse != "access_token") ||
		(claims.TenantID != "" && claims.TenantID != verifier.tenantID) {
		return ErrInvalidServiceToken
	}
	return nil
}

func containsAudience(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
