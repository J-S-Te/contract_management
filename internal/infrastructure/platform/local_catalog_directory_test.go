package platform

import (
	"context"
	"github.com/j-s-te/contract-management/internal/application"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestCatalogTargetsUseBoundOwnerDirectory 验证目录目标与组织均来自同租户有效平台目录。
// 参数：t为测试上下文，HTTP边界为本机受控服务器。
// 返回值：无；任意ID、越租户或丢失角色过滤时失败。
func TestCatalogTargetsUseBoundOwnerDirectory(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			r.ParseForm()
			if r.Form.Get("scope") != "owner_directory.read" {
				t.Fatal(r.Form)
			}
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"owner_directory.read"}`))
			return
		}
		calls++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatal(r.Header)
		}
		if r.URL.Query().Get("user_id") == "" && r.URL.Query().Get("role_code") != "sales" {
			t.Fatal(r.URL)
		}
		w.Write([]byte(`{"data":{"items":[{"user_id":"sales-valid","display_name":"有效销售","organizations":[{"organization_id":"org-valid","organization_name":"有效组织"}]}],"page":1,"page_size":50,"total":1}}`))
	}))
	defer server.Close()
	directory := NewLocalCatalogTargetDirectory(NewPersonnelDirectory(server.URL, "client", "test-secret", time.Second), "tenant-a")
	actor := application.Principal{TenantID: "tenant-a", IdentityID: "sales-valid"}
	targets, err := directory.ListCatalogTargets(context.Background(), actor)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	orgs, err := directory.CurrentCatalogOrganizations(context.Background(), actor)
	if err != nil || len(orgs) != 1 || orgs[0] != "org-valid" {
		t.Fatalf("orgs=%v err=%v", orgs, err)
	}
	actor.TenantID = "tenant-foreign"
	before := calls
	if _, err = directory.ListCatalogTargets(context.Background(), actor); err == nil || calls != before {
		t.Fatal("越租户请求触达目录")
	}
}

// TestCatalogDirectoryRejectsMalformedSuccess 验证平台异常200不能被伪装为空候选目录。
// 参数：t为测试上下文，边界服务返回缺少可信数据外壳的响应。
// 返回值：无；异常协议被当作成功时失败。
func TestCatalogDirectoryRejectsMalformedSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			w.Write([]byte(`{"access_token":"token","token_type":"Bearer","scope":"owner_directory.read"}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	d := NewLocalCatalogTargetDirectory(NewPersonnelDirectory(server.URL, "client", "test", time.Second), "tenant-a")
	if _, err := d.ListCatalogTargets(context.Background(), application.Principal{TenantID: "tenant-a"}); err == nil {
		t.Fatal("缺少目录外壳被当作空成功")
	}
}
