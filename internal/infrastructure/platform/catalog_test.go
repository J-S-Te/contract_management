package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCatalogPayloadFromManifestUsesCanonicalRoleCodes(t *testing.T) {
	payload, err := catalogPayloadFromManifest([]byte(`
metadata:
  version: 3
permissions:
  - code: contract.read
    name: 查看合同
    resource: contract
    action: read
    risk_level: LOW
roles:
  - code: admin
    name: 管理员
    permissions: [contract.read]
`))
	if err != nil {
		t.Fatalf("catalogPayloadFromManifest() error = %v", err)
	}
	if payload.CatalogVersion != "3" || len(payload.Permissions) != 1 || len(payload.Roles) != 1 ||
		payload.Roles[0].Code != "admin" || payload.Permissions[0].ResourceCode != "contract" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestSyncAuthorizationCatalogUsesPublisherCredential(t *testing.T) {
	var published catalogPayload
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth2/token":
			clientID, secret, ok := request.BasicAuth()
			if !ok || clientID != "publisher" || secret != "secret" {
				t.Fatal("catalog token request did not use publisher basic authentication")
			}
			if request.FormValue("grant_type") != "client_credentials" || request.FormValue("scope") != "authorization.catalog.sync" {
				t.Fatal("unexpected catalog token form")
			}
			writeJSONResponse(writer, map[string]any{"access_token": "token", "token_type": "Bearer", "scope": "authorization.catalog.sync"})
		case "/api/v1/applications/app-1/authorization-catalog":
			if request.Method != http.MethodPut || request.Header.Get("Authorization") != "Bearer token" {
				t.Fatal("unexpected catalog publication request")
			}
			if err := json.NewDecoder(request.Body).Decode(&published); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			writeJSONResponse(writer, map[string]any{"sync_status": "SYNCED"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	err := SyncAuthorizationCatalog(context.Background(), CatalogSyncOptions{
		Enabled: true, BaseURL: server.URL, ApplicationID: "app-1",
		ClientID: "publisher", ClientSecret: "secret",
	})
	if err != nil {
		t.Fatalf("SyncAuthorizationCatalog() error = %v", err)
	}
	if published.CatalogVersion != "9" || len(published.Permissions) == 0 || len(published.Roles) == 0 {
		t.Fatalf("published payload = %#v", published)
	}
}

func writeJSONResponse(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}
