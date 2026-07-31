package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/authz"
	"gopkg.in/yaml.v3"
)

type CatalogSyncOptions struct {
	Enabled       bool
	BaseURL       string
	ApplicationID string
	ClientID      string
	ClientSecret  string
}

type permissionManifest struct {
	Metadata struct {
		Version int `yaml:"version"`
	} `yaml:"metadata"`
	Permissions []struct {
		Code      string `yaml:"code"`
		Name      string `yaml:"name"`
		Resource  string `yaml:"resource"`
		Action    string `yaml:"action"`
		RiskLevel string `yaml:"risk_level"`
	} `yaml:"permissions"`
	Roles []struct {
		Code        string   `yaml:"code"`
		Name        string   `yaml:"name"`
		Permissions []string `yaml:"permissions"`
	} `yaml:"roles"`
}

type catalogPayload struct {
	CatalogVersion string              `json:"catalog_version"`
	Permissions    []catalogPermission `json:"permissions"`
	Roles          []catalogRole       `json:"roles"`
}

type catalogPermission struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	Action       string `json:"action"`
	ResourceCode string `json:"resource_code"`
	ResourceName string `json:"resource_name"`
	RiskLevel    string `json:"risk_level"`
}

type catalogRole struct {
	Code        string   `json:"code"`
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
}

// SyncAuthorizationCatalog publishes the embedded application-owned manifest to the platform.
// Enabling synchronization makes a failed publication a startup error so the API never runs with
// a role catalog that differs from its own authorization checks.
func SyncAuthorizationCatalog(ctx context.Context, options CatalogSyncOptions) error {
	if !options.Enabled {
		return nil
	}
	payload, err := catalogPayloadFromManifest(authz.PermissionManifest)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	token, err := catalogAccessToken(ctx, client, options)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode authorization catalog: %w", err)
	}
	endpoint := strings.TrimRight(options.BaseURL, "/") + "/api/v1/applications/" + url.PathEscape(options.ApplicationID) + "/authorization-catalog"
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create authorization catalog request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("publish authorization catalog: %w", err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("platform authorization catalog returned %d", response.StatusCode)
	}
	return nil
}

func catalogAccessToken(ctx context.Context, client *http.Client, options CatalogSyncOptions) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"authorization.catalog.sync"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(options.BaseURL, "/")+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("create authorization catalog token request: %w", err)
	}
	request.SetBasicAuth(options.ClientID, options.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("request authorization catalog token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return "", fmt.Errorf("platform authorization catalog token returned %d", response.StatusCode)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode authorization catalog token: %w", err)
	}
	if result.AccessToken == "" || !strings.EqualFold(result.TokenType, "bearer") || !hasScope(result.Scope, "authorization.catalog.sync") {
		return "", fmt.Errorf("platform token missing authorization.catalog.sync scope")
	}
	return result.AccessToken, nil
}

func catalogPayloadFromManifest(raw []byte) (catalogPayload, error) {
	var manifest permissionManifest
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return catalogPayload{}, fmt.Errorf("decode authorization manifest: %w", err)
	}
	if manifest.Metadata.Version <= 0 || len(manifest.Permissions) == 0 || len(manifest.Roles) == 0 {
		return catalogPayload{}, fmt.Errorf("authorization manifest is incomplete")
	}
	payload := catalogPayload{CatalogVersion: strconv.Itoa(manifest.Metadata.Version)}
	for _, permission := range manifest.Permissions {
		payload.Permissions = append(payload.Permissions, catalogPermission{
			Code: permission.Code, Name: permission.Name, Action: permission.Action,
			ResourceCode: permission.Resource, ResourceName: permission.Resource, RiskLevel: permission.RiskLevel,
		})
	}
	for _, role := range manifest.Roles {
		payload.Roles = append(payload.Roles, catalogRole{Code: role.Code, Name: role.Name, Permissions: role.Permissions})
	}
	return payload, nil
}
