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
	// 版本随 manifest 演进，断言两者一致而不是写死数字，避免每次升版都改测试。
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		t.Fatalf("LoadAuthorizationCatalog() error = %v", err)
	}
	if published.CatalogVersion != catalog.Version || len(published.Permissions) == 0 || len(published.Roles) == 0 {
		t.Fatalf("published payload = %#v", published)
	}
}

func writeJSONResponse(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

// 业务管理员需要通过"已审批合同"创建项目，但只该拿到这一个读权限：
// 多给任何一条都会把合同系统内的操作能力泄露给项目侧角色。
func TestContractImporterRoleGrantsOnlyApprovedRead(t *testing.T) {
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		t.Fatalf("LoadAuthorizationCatalog() error = %v", err)
	}
	permissions, ok := catalog.RolePermissions["contract_importer"]
	if !ok {
		t.Fatal("contract_importer role is missing from the manifest")
	}
	if len(permissions) != 1 {
		t.Fatalf("contract_importer permissions = %v, want exactly one", permissions)
	}
	if _, ok := permissions["contract.approved.read"]; !ok {
		t.Fatalf("contract_importer must grant contract.approved.read, got %v", permissions)
	}
	for _, forbidden := range []string{
		"contract.read", "contract.create", "contract.edit",
		"contract.document.download", "contract.stamped_pdf.upload", "contract.signing.manage",
		"approval.view", "approval.process", "approval.manage", "approval_rule.manage",
	} {
		if _, granted := permissions[forbidden]; granted {
			t.Fatalf("contract_importer must not grant %s", forbidden)
		}
	}
}

// 编号规则是全系统单行配置（无 tenant_id），因此写入权限只能挂在专用角色上：
// 一旦它出现在 admin、contract_specialist 等既有租户级角色里，任一租户的合同管理员
// 就能改全系统前缀，跨租户影响所有租户后续签发。这里把该边界钉在清单上。
func TestNumberRuleManagePermissionIsGrantedOnlyByDedicatedRole(t *testing.T) {
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		t.Fatalf("LoadAuthorizationCatalog() error = %v", err)
	}
	const permission = "contract.number_rule.manage"
	if _, ok := catalog.Permissions[permission]; !ok {
		t.Fatalf("manifest is missing permission %s", permission)
	}
	if _, ok := catalog.Roles["number_rule_admin"]; !ok {
		t.Fatal("manifest is missing role number_rule_admin")
	}
	granted, ok := catalog.RolePermissions["number_rule_admin"]
	if !ok || len(granted) != 1 {
		t.Fatalf("number_rule_admin permissions = %v, want exactly one", granted)
	}
	if _, ok := granted[permission]; !ok {
		t.Fatalf("number_rule_admin must grant %s, got %v", permission, granted)
	}
	for roleCode, rolePermissions := range catalog.RolePermissions {
		if roleCode == "number_rule_admin" {
			continue
		}
		if _, granted := rolePermissions[permission]; granted {
			t.Fatalf("role %s must not grant %s: 编号规则写入不得退回租户级角色旁路", roleCode, permission)
		}
	}
}
