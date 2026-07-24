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

	"github.com/j-s-te/contract-management/internal/application"
)

var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator struct {
	BaseURL           string
	SessionCookieName string
	Client            *http.Client
}

func NewAuthenticator(baseURL, sessionCookieName string) *Authenticator {
	return &Authenticator{BaseURL: strings.TrimRight(baseURL, "/"), SessionCookieName: sessionCookieName, Client: &http.Client{Timeout: 5 * time.Second}}
}

type principalEnvelope struct {
	Code string `json:"code"`
	Data struct {
		Tenant          reference `json:"tenant"`
		User            reference `json:"user"`
		PermissionCodes []string  `json:"permission_codes"`
	} `json:"data"`
}
type reference struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (a *Authenticator) Authenticate(ctx context.Context, incoming *http.Request) (application.Principal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/api/v1/auth/me", nil)
	if err != nil {
		return application.Principal{}, err
	}
	if a.SessionCookieName == "" {
		a.SessionCookieName = "bp_session"
	}
	cookie, err := incoming.Cookie(a.SessionCookieName)
	if err != nil {
		return application.Principal{}, ErrUnauthenticated
	}
	req.AddCookie(cookie)
	if requestID := incoming.Header.Get("X-Request-ID"); requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return application.Principal{}, fmt.Errorf("platform auth unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, resp.Body)
		return application.Principal{}, ErrUnauthenticated
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return application.Principal{}, fmt.Errorf("platform auth returned %d", resp.StatusCode)
	}
	var envelope principalEnvelope
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&envelope); err != nil {
		return application.Principal{}, err
	}
	if envelope.Code != "OK" || envelope.Data.Tenant.ID == "" || envelope.Data.User.ID == "" {
		return application.Principal{}, ErrUnauthenticated
	}
	permissions := make(map[string]bool, len(envelope.Data.PermissionCodes)*2)
	for _, permission := range envelope.Data.PermissionCodes {
		permissions[permission] = true
		// Mirror platform-prefixed codes without the prefix and convert the
		// remaining ":" separators to "." so internal permission checks like
		// Has("contract.create") can match the platform-issued
		// "platform:contract:create" code. This lets basic-platform roles
		// (e.g. platform-super-admin) transparently cover contract_management
		// permission gates without requiring a duplicate IAM grant.
		if strings.HasPrefix(permission, "platform:") {
			permissions[strings.ReplaceAll(strings.TrimPrefix(permission, "platform:"), ":", ".")] = true
		}
	}
	return application.Principal{TenantID: envelope.Data.Tenant.ID, UserID: envelope.Data.User.ID, Permissions: permissions}, nil
}
