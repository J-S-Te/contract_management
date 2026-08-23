package notification

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// NewClientCredentialsTokenSource 构造带 notification.ingest scope 的平台机器令牌源。
// 任一参数为空时返回 nil，投递将不携带令牌（仅限未启用投递的本地环境）。
func NewClientCredentialsTokenSource(ctx context.Context, tokenURL, clientID, clientSecret string) func(context.Context) (string, error) {
	if strings.TrimSpace(tokenURL) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil
	}
	credentials := clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		AuthStyle:    oauth2.AuthStyleInHeader,
		Scopes:       []string{"notification.ingest"},
	}
	tokenContext := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 10 * time.Second})
	source := credentials.TokenSource(tokenContext)
	return func(ctx context.Context) (string, error) {
		token, err := source.Token()
		if err != nil {
			return "", err
		}
		if token == nil || strings.TrimSpace(token.AccessToken) == "" {
			return "", errors.New("notification token response has no access_token")
		}
		return token.AccessToken, nil
	}
}
