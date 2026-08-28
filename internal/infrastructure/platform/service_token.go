package platform

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ErrInvalidServiceToken 区分机器令牌错误与浏览器会话错误；机器认证失败时禁止回退到请求头身份。
var ErrInvalidServiceToken = errors.New("invalid service token")

// ClientCredentialsTokenVerifier 校验基础平台签发的 application JWT。
type ClientCredentialsTokenVerifier interface {
	VerifyClientCredentials(context.Context, string) (ServiceTokenIdentity, error)
}

// ServiceTokenIdentity 是经过平台签名和完整调用方绑定校验后的机器身份。
type ServiceTokenIdentity struct {
	TenantID        string
	ApplicationCode string
	EnvironmentCode string
}

// ClientCredentialsVerifierOptions 定义本地接收端接受的唯一机器信任域。
type ClientCredentialsVerifierOptions struct {
	Issuer, Audience, PublicKeyPath, ClientID, TenantID string
	CallerApplicationCode, CallerEnvironmentCode        string
	RequiredScope                                       string
}

type platformApplicationTokenVerifier struct {
	publicKey                        ed25519.PublicKey
	issuer, audience                 string
	clientID, tenantID               string
	applicationCode, environmentCode string
	requiredScope                    string
}

type platformApplicationTokenClaims struct {
	Issuer          string   `json:"iss"`
	Audience        string   `json:"aud"`
	TokenUse        string   `json:"token_use"`
	Subject         string   `json:"sub"`
	OAuthClientID   string   `json:"oauth_client_id"`
	TenantID        string   `json:"tenant_id"`
	ApplicationCode string   `json:"application_code"`
	EnvironmentCode string   `json:"environment_code"`
	Scopes          []string `json:"scope"`
	IssuedAt        int64    `json:"iat"`
	NotBefore       int64    `json:"nbf"`
	ExpiresAt       int64    `json:"exp"`
}

// NewClientCredentialsTokenVerifier 从只读公钥构建平台机器令牌验证器。
// options 缺失或公钥无效时返回错误；成功时返回失败关闭的本地验证器。
func NewClientCredentialsTokenVerifier(_ context.Context, options ClientCredentialsVerifierOptions) (ClientCredentialsTokenVerifier, error) {
	values := []string{options.Issuer, options.Audience, options.PublicKeyPath, options.ClientID, options.TenantID, options.CallerApplicationCode, options.CallerEnvironmentCode, options.RequiredScope}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%w: machine token trust binding is incomplete", ErrInvalidServiceToken)
		}
	}
	publicKey, err := loadApplicationPublicKey(options.PublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidServiceToken, err)
	}
	return &platformApplicationTokenVerifier{
		publicKey: publicKey, issuer: options.Issuer, audience: options.Audience,
		clientID: options.ClientID, tenantID: options.TenantID,
		applicationCode: options.CallerApplicationCode, environmentCode: options.CallerEnvironmentCode,
		requiredScope: options.RequiredScope,
	}, nil
}

func (verifier *platformApplicationTokenVerifier) VerifyClientCredentials(_ context.Context, rawToken string) (ServiceTokenIdentity, error) {
	parts := strings.Split(rawToken, ".")
	if verifier == nil || len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := decodeApplicationTokenJSON(parts[0], &header); err != nil || header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(verifier.publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	claims := platformApplicationTokenClaims{}
	if err := decodeApplicationTokenJSON(parts[1], &claims); err != nil || !verifier.validClaims(claims, time.Now().UTC()) {
		return ServiceTokenIdentity{}, ErrInvalidServiceToken
	}
	return ServiceTokenIdentity{TenantID: claims.TenantID, ApplicationCode: claims.ApplicationCode, EnvironmentCode: claims.EnvironmentCode}, nil
}

func (verifier *platformApplicationTokenVerifier) validClaims(claims platformApplicationTokenClaims, now time.Time) bool {
	return claims.Issuer == verifier.issuer && claims.Audience == verifier.audience && claims.TokenUse == "application" &&
		claims.Subject == verifier.clientID && claims.OAuthClientID != "" && claims.TenantID == verifier.tenantID &&
		claims.ApplicationCode == verifier.applicationCode && claims.EnvironmentCode == verifier.environmentCode &&
		len(claims.Scopes) == 1 && claims.Scopes[0] == verifier.requiredScope && claims.IssuedAt > 0 &&
		claims.NotBefore >= claims.IssuedAt && claims.ExpiresAt > claims.NotBefore &&
		!time.Unix(claims.NotBefore, 0).After(now.Add(time.Minute)) && time.Unix(claims.ExpiresAt, 0).After(now)
}

func loadApplicationPublicKey(path string) (ed25519.PublicKey, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, remainder := pem.Decode(contents)
	if block == nil || len(bytes.TrimSpace(remainder)) != 0 {
		return nil, errors.New("application JWT public key must contain exactly one PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok || len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("application JWT public key must be an Ed25519 PKIX key")
	}
	return publicKey, nil
}

func decodeApplicationTokenJSON(encoded string, destination any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("application JWT JSON contains multiple values")
	}
	return nil
}
