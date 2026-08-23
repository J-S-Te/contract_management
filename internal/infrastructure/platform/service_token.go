package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrInvalidServiceToken is deliberately distinct from a browser session
// error: callers must never fall back to routing headers when a bearer token
// was supplied but cannot be verified.
var ErrInvalidServiceToken = errors.New("invalid service token")

// ClientCredentialsTokenVerifier verifies the Keycloak access token used by a
// trusted service-to-service caller. It verifies signature, issuer, expiry,
// token type, authorized party, audience and tenant; it does not accept an ID
// token. The returned identity carries the tenant that the routing layer must
// trust instead of a caller-supplied header.
type ClientCredentialsTokenVerifier interface {
	VerifyClientCredentials(context.Context, string) (ServiceTokenIdentity, error)
}

// ServiceTokenIdentity 是经过签名、issuer、audience 及调用方绑定校验后的机器身份。
// 路由层必须使用这里的租户，不能再从可由请求方任意填写的请求头推导租户边界。
type ServiceTokenIdentity struct {
	TenantID string
}

type ClientCredentialsVerifierOptions struct {
	Issuer, BackchannelBaseURL, ClientID, Audience, TenantID string
	Timeout                                                  time.Duration
}

type keycloakClientCredentialsVerifier struct {
	verifier *oidc.IDTokenVerifier
	clientID string
	audience string
	tenantID string
}

type serviceTokenClaims struct {
	AuthorizedParty string `json:"azp"`
	ClientID        string `json:"client_id"`
	Type            string `json:"typ"`
	TokenUse        string `json:"token_use"`
	TenantID        string `json:"tenant_id"`
}

func NewClientCredentialsTokenVerifier(ctx context.Context, options ClientCredentialsVerifierOptions) (ClientCredentialsTokenVerifier, error) {
	issuer := strings.TrimRight(strings.TrimSpace(options.Issuer), "/")
	backchannel := strings.TrimRight(strings.TrimSpace(options.BackchannelBaseURL), "/")
	clientID := strings.TrimSpace(options.ClientID)
	audience := strings.TrimSpace(options.Audience)
	tenantID := strings.TrimSpace(options.TenantID)
	if issuer == "" || clientID == "" || audience == "" || tenantID == "" {
		return nil, fmt.Errorf("%w: issuer, client ID, audience and tenant are required", ErrInvalidServiceToken)
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Second
	}
	client := &http.Client{Timeout: options.Timeout}
	if backchannel != "" {
		publicURL, err := url.Parse(issuer)
		if err != nil {
			return nil, fmt.Errorf("%w: issuer: %v", ErrInvalidServiceToken, err)
		}
		backchannelURL, err := url.Parse(backchannel)
		if err != nil {
			return nil, fmt.Errorf("%w: backchannel: %v", ErrInvalidServiceToken, err)
		}
		client.Transport = &backchannelTransport{base: http.DefaultTransport, public: publicURL, backchannel: backchannelURL}
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: load discovery: %v", ErrInvalidServiceToken, err)
	}
	// Access tokens are issued for the machine-client audience, not the
	// browser RP Client ID. The audience is validated explicitly below.
	return &keycloakClientCredentialsVerifier{
		verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		clientID: clientID,
		audience: audience,
		tenantID: tenantID,
	}, nil
}

func (v *keycloakClientCredentialsVerifier) VerifyClientCredentials(ctx context.Context, rawToken string) (ServiceTokenIdentity, error) {
	if v == nil || v.verifier == nil || strings.TrimSpace(rawToken) == "" {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return ServiceTokenIdentity{}, fmt.Errorf("%w: signature, issuer or expiry: %v", ErrInvalidServiceToken, err)
	}
	claims := serviceTokenClaims{}
	if err := token.Claims(&claims); err != nil {
		return ServiceTokenIdentity{}, fmt.Errorf("%w: claims: %v", ErrInvalidServiceToken, err)
	}
	if err := v.validateClaims(claims, token.Audience); err != nil {
		return ServiceTokenIdentity{}, err
	}
	return ServiceTokenIdentity{TenantID: v.tenantID}, nil
}

func (v *keycloakClientCredentialsVerifier) validateClaims(claims serviceTokenClaims, audiences []string) error {
	if !strings.EqualFold(strings.TrimSpace(claims.Type), "bearer") {
		return fmt.Errorf("%w: token typ is not bearer", ErrInvalidServiceToken)
	}
	// Keycloak service-account access tokens do not include token_use unless a
	// custom mapper is installed. When present it must still identify an access
	// token; typ + signature/issuer/expiry + azp/audience remain mandatory.
	if tokenUse := strings.TrimSpace(claims.TokenUse); tokenUse != "" && tokenUse != "access_token" {
		return fmt.Errorf("%w: token_use is not access_token", ErrInvalidServiceToken)
	}
	// azp is the authenticated Keycloak caller.  Do not fall back to client_id:
	// the latter is an optional mapper and is not the authorized-party contract.
	if strings.TrimSpace(claims.AuthorizedParty) != v.clientID || !containsAudience(audiences, v.audience) {
		return fmt.Errorf("%w: authorized party or audience", ErrInvalidServiceToken)
	}
	if clientID := strings.TrimSpace(claims.ClientID); clientID != "" && clientID != v.clientID {
		return fmt.Errorf("%w: client_id", ErrInvalidServiceToken)
	}
	// 租户边界由服务端配置绑定。若 Keycloak 安装了 tenant_id mapper，则额外
	// 校验 claim；默认 service-account token 缺少该 claim 时不再错误拒绝。
	if tenantID := strings.TrimSpace(claims.TenantID); tenantID != "" && tenantID != v.tenantID {
		return fmt.Errorf("%w: tenant", ErrInvalidServiceToken)
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
