package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

// PersonnelDirectoryClient uses the platform's machine-only owner directory.
// Its client credential must have only owner_directory.read.
type PersonnelDirectoryClient struct {
	baseURL, clientID, clientSecret string
	client                          *http.Client
}

func NewPersonnelDirectory(baseURL, clientID, clientSecret string, timeout time.Duration) application.PersonnelDirectory {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(clientSecret) == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &PersonnelDirectoryClient{baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID, clientSecret: clientSecret, client: &http.Client{Timeout: timeout}}
}

func (c *PersonnelDirectoryClient) ListEligibleUsers(ctx context.Context, actor application.Principal, roleCodes []string) ([]application.UserReference, error) {
	if c == nil || c.client == nil || strings.TrimSpace(actor.TenantID) == "" || len(roleCodes) == 0 {
		return nil, fmt.Errorf("personnel directory request is invalid")
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	users := map[string]*application.UserReference{}
	for _, rawRole := range roleCodes {
		role := strings.TrimSpace(rawRole)
		if role == "" {
			return nil, fmt.Errorf("personnel role is invalid")
		}
		for page := 1; ; page++ {
			query := url.Values{"role_code": {role}, "page": {fmt.Sprint(page)}, "page_size": {"50"}}
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/v1/internal/owner-directory?"+query.Encode(), nil)
			if requestErr != nil {
				return nil, requestErr
			}
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Accept", "application/json")
			response, requestErr := c.client.Do(request)
			if requestErr != nil {
				return nil, fmt.Errorf("query personnel directory: %w", requestErr)
			}
			var envelope struct {
				Data struct {
					Items []struct {
						UserID      string `json:"user_id"`
						DisplayName string `json:"display_name"`
					} `json:"items"`
					Page     int   `json:"page"`
					PageSize int   `json:"page_size"`
					Total    int64 `json:"total"`
				} `json:"data"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("platform personnel directory returned %d", response.StatusCode)
			}
			if decodeErr != nil {
				return nil, fmt.Errorf("decode personnel directory: %w", decodeErr)
			}
			for _, item := range envelope.Data.Items {
				id, name := strings.TrimSpace(item.UserID), strings.TrimSpace(item.DisplayName)
				if id == "" || name == "" {
					return nil, fmt.Errorf("platform personnel directory returned an incomplete user")
				}
				user := users[id]
				if user == nil {
					user = &application.UserReference{UserID: id, DisplayName: name}
					users[id] = user
				}
				user.Roles = appendUniquePersonnelRole(user.Roles, role)
			}
			if int64(page*50) >= envelope.Data.Total || len(envelope.Data.Items) == 0 {
				break
			}
			if page >= 100 {
				return nil, fmt.Errorf("platform personnel directory exceeded page limit")
			}
		}
	}
	result := make([]application.UserReference, 0, len(users))
	for _, user := range users {
		sort.Strings(user.Roles)
		result = append(result, *user)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].DisplayName == result[j].DisplayName {
			return result[i].UserID < result[j].UserID
		}
		return result[i].DisplayName < result[j].DisplayName
	})
	return result, nil
}

func (c *PersonnelDirectoryClient) accessToken(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"owner_directory.read"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.SetBasicAuth(c.clientID, c.clientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request personnel directory token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", fmt.Errorf("platform personnel token returned %d", response.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil {
		return "", err
	}
	if token.AccessToken == "" || !strings.EqualFold(token.TokenType, "bearer") || !hasScope(token.Scope, "owner_directory.read") {
		return "", fmt.Errorf("platform token missing owner_directory.read scope")
	}
	return token.AccessToken, nil
}

func appendUniquePersonnelRole(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}
