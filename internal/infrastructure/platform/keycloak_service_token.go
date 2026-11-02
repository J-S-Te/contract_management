package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// KeycloakClientCredentialsVerifierOptions 仅用于尚未迁移的内部接口。
// RequiredScope 是机器身份唯一允许的范围：令牌的范围集合必须恰好等于该值，
// 少一个或额外多一个都拒绝，与平台 application token 校验器同一口径。
type KeycloakClientCredentialsVerifierOptions struct {
	Issuer, BackchannelBaseURL, ClientID, Audience, TenantID, RequiredScope string
	Timeout                                                                 time.Duration
}

type keycloakClientCredentialsVerifier struct {
	verifier                                    *oidc.IDTokenVerifier
	clientID, audience, tenantID, requiredScope string
}

type serviceTokenClaims struct {
	AuthorizedParty string             `json:"azp"`
	ClientID        string             `json:"client_id"`
	Type            string             `json:"typ"`
	TokenUse        string             `json:"token_use"`
	TenantID        string             `json:"tenant_id"`
	Scopes          serviceTokenScopes `json:"scope"`
}

// serviceTokenScopes 兼容 Keycloak 的两种范围声明形态：
// 标准 Keycloak 访问令牌是空格分隔字符串（"scope": "a b"），
// 部分兼容实现直接签发数组（"scope": ["a","b"]）。
// 两种形态都收敛成切片后按“唯一范围”判定，避免形态差异放宽校验。
type serviceTokenScopes []string

func (scopes *serviceTokenScopes) UnmarshalJSON(raw []byte) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		*scopes = nil
		return nil
	}
	if trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		*scopes = serviceTokenScopes(strings.Fields(value))
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	*scopes = serviceTokenScopes(values)
	return nil
}

// NewKeycloakClientCredentialsTokenVerifier 构建旧 Keycloak 机器域验证器。
// 该构造器只服务尚未迁移的路由，禁止用于新的内部接口。
// RequiredScope 缺失时直接失败关闭，避免退回只校验 client_id/audience 的弱断言。
func NewKeycloakClientCredentialsTokenVerifier(ctx context.Context, options KeycloakClientCredentialsVerifierOptions) (ClientCredentialsTokenVerifier, error) {
	issuer := strings.TrimRight(strings.TrimSpace(options.Issuer), "/")
	requiredScope := strings.TrimSpace(options.RequiredScope)
	if issuer == "" || options.ClientID == "" || options.Audience == "" || options.TenantID == "" || requiredScope == "" {
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
	return &keycloakClientCredentialsVerifier{
		verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}),
		clientID: options.ClientID, audience: options.Audience, tenantID: options.TenantID, requiredScope: requiredScope,
	}, nil
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
	// 机器身份只承载唯一用途：范围缺失、多出任何范围或指向其它用途都拒绝，
	// 与平台 application token 校验器的 len==1 && scopes[0]==required 口径一致。
	if len(claims.Scopes) != 1 || claims.Scopes[0] != verifier.requiredScope {
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
