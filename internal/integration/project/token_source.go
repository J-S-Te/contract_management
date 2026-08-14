package project

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// NewClientCredentialsTokenSource 为内部投递构造平台/Keycloak 机器令牌源（client_credentials）。
// audience 非空时作为 token 请求参数携带；所有参数未配置时返回 nil，投递将不带令牌。
func NewClientCredentialsTokenSource(ctx context.Context, tokenURL, clientID, clientSecret, audience string) func(context.Context) (string, error) {
	if strings.TrimSpace(tokenURL) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil
	}
	params := url.Values{}
	if strings.TrimSpace(audience) != "" {
		params.Set("audience", strings.TrimSpace(audience))
	}
	credentials := clientcredentials.Config{
		ClientID: clientID, ClientSecret: clientSecret, TokenURL: tokenURL,
		AuthStyle: oauth2.AuthStyleInHeader, EndpointParams: params,
	}
	tokenContext := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 10 * time.Second})
	source := credentials.TokenSource(tokenContext)
	return func(ctx context.Context) (string, error) {
		token, err := source.Token()
		if err != nil {
			return "", err
		}
		if token == nil || strings.TrimSpace(token.AccessToken) == "" {
			return "", errors.New("project integration token response has no access_token")
		}
		return token.AccessToken, nil
	}
}
