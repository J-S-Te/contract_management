package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/authorizationrefresh"
)

var (
	ErrAuthorizationUnauthorized = errors.New("authorization context token is unauthorized")
	ErrAuthorizationForbidden    = errors.New("application authorization is forbidden")
	ErrAuthorizationUnavailable  = errors.New("authorization context service is unavailable")
	ErrAuthorizationInvalid      = errors.New("authorization context is invalid")
)

type AuthorizationDataScope struct {
	RoleCode        string `json:"role_code"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	EnvironmentCode string `json:"environment_code"`
}

type AuthorizationContext struct {
	Subject                    string                   `json:"sub"`
	SubjectID                  string                   `json:"subject_id"`
	IdentityID                 string                   `json:"identity_id"`
	TenantID                   string                   `json:"tenant_id"`
	PersonID                   string                   `json:"person_id"`
	ClientID                   string                   `json:"client_id"`
	ApplicationCode            string                   `json:"application_code"`
	EnvironmentCode            string                   `json:"environment_code"`
	Roles                      []string                 `json:"roles"`
	Permissions                []string                 `json:"permissions"`
	DataScopes                 []AuthorizationDataScope `json:"data_scopes"`
	CatalogVersion             string                   `json:"catalog_version"`
	CompatibleCatalogVersions  []string                 `json:"compatible_catalog_versions"`
	RoleConfigHash             string                   `json:"role_config_hash"`
	CompatibleRoleConfigHashes []string                 `json:"compatible_role_config_hashes"`
	AuthorizationRevision      uint64                   `json:"authorization_revision"`
}

type AuthorizationContextClient interface {
	Resolve(context.Context, string) (AuthorizationContext, error)
}

type HTTPAuthorizationContextClient struct {
	endpoint string
	client   *http.Client
}

func NewHTTPAuthorizationContextClient(endpoint string, timeout time.Duration, transport http.RoundTripper) (*HTTPAuthorizationContextClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" || timeout <= 0 {
		return nil, errors.New("authorization context endpoint and timeout are required")
	}
	client := &http.Client{Timeout: timeout}
	if transport != nil {
		client.Transport = transport
	}
	return &HTTPAuthorizationContextClient{endpoint: endpoint, client: client}, nil
}

// Resolve 在线读取平台授权并传播页面新鲜读取要求。
// 参数：ctx 为请求上下文，accessToken 为服务端持有的访问令牌。
// 返回值：当前授权快照或认证、授权、依赖分类错误。
func (c *HTTPAuthorizationContextClient) Resolve(ctx context.Context, accessToken string) (AuthorizationContext, error) {
	if strings.TrimSpace(accessToken) == "" {
		return AuthorizationContext{}, ErrAuthorizationUnauthorized
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return AuthorizationContext{}, fmt.Errorf("%w: create request: %v", ErrAuthorizationUnavailable, err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	if authorizationrefresh.Forced(ctx) {
		request.Header.Set(authorizationrefresh.Header, "1")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return AuthorizationContext{}, fmt.Errorf("%w: %v", ErrAuthorizationUnavailable, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return AuthorizationContext{}, ErrAuthorizationUnauthorized
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return AuthorizationContext{}, ErrAuthorizationForbidden
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode >= 500 {
			return AuthorizationContext{}, fmt.Errorf("%w: HTTP %d", ErrAuthorizationUnavailable, response.StatusCode)
		}
		return AuthorizationContext{}, fmt.Errorf("%w: HTTP %d", ErrAuthorizationInvalid, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	var result AuthorizationContext
	if err := decoder.Decode(&result); err != nil {
		return AuthorizationContext{}, fmt.Errorf("%w: decode response: %v", ErrAuthorizationInvalid, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return AuthorizationContext{}, fmt.Errorf("%w: response contains trailing JSON", ErrAuthorizationInvalid)
	}
	return result, nil
}
