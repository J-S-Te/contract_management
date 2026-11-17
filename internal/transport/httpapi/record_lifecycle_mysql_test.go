package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	store "github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// recordHTTPFixture 用途：以真实仓储和安全中间件搭建隔离 API 场景。
// 参数：t 为测试上下文，DSN 必须显式指向回环 contract_test_* 库。
// 返回值：路由、业务服务、合成管理员与合同；身份替身不代表真实 SSO 验收。
func recordHTTPFixture(t *testing.T) (http.Handler, *application.Service, application.Principal, contract.Contract) {
	t.Helper()
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is not configured; no database connection attempted")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" ||
		!(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("record HTTP tests require an explicit loopback contract_test_* database")
	}
	cfg.ParseTime = true
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err = migration.Run(ctx, cfg.FormatDSN(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	tenant, user, identity := ulid.Make().String(), ulid.Make().String(), ulid.Make().String()
	t.Cleanup(func() {
		for _, table := range []string{"con_audit_outbox", "con_contract_responsibility_outbox", "con_contract_responsibility_stream", "con_contract_lifecycle_event", "con_contract"} {
			if err := db.Exec("DELETE FROM "+table+" WHERE tenant_id=?", tenant).Error; err != nil {
				t.Error("isolated fixture cleanup failed")
			}
		}
		_ = pool.Close()
	})
	actor := application.Principal{TenantID: tenant, UserID: user, IdentityID: identity, Roles: []string{"admin"},
		Permissions:      map[string]bool{"contract.read": true, "contract.delete": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}, "contract.delete": {AllowAll: true}}}
	repository := store.NewRepository(db)
	service := &application.Service{Repo: repository}
	c := contract.Contract{ID: ulid.Make().String(), TenantID: tenant, Title: "合成HTTP验收合同", Type: "直签", ServiceType: "软件测试",
		OwnerUserID: user, OwnerIdentityID: identity, OwnerDisplayName: "合成负责人", AmountMinor: 10000, Currency: "CNY", Content: "合成正文", Status: contract.StatusDraft, Version: 1}
	if err = repository.CreateContract(ctx, c, user); err != nil {
		t.Fatal(err)
	}
	identityProvider := identityFunc(func(context.Context, *http.Request) (application.Principal, error) { return actor, nil })
	return NewRouter(service, identityProvider, nil), service, actor, c
}

// recordHTTPRequest 用途：通过真实 Gin 路由发送同源管理请求并解码标准响应。
// 参数：t/router 为场景，method/path/body 为请求，target 为可选响应数据接收者。
// 返回值：HTTP 状态；响应解码失败立即终止当前场景。
func recordHTTPRequest(t *testing.T, router http.Handler, method, path, body string, target any) int {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if target != nil && response.Code == http.StatusOK {
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(envelope.Data, target); err != nil {
			t.Fatal(err)
		}
	}
	return response.Code
}

// TestContractRecordDeleteHTTPMySQL 用途：验证一份草稿从预检到删除、隐藏和幂等回执的真实 API 链。
// 参数：t 为隔离 MySQL 最小场景，角色来自明确标识的测试身份替身。
// 返回值：无；HTTP 状态、记录版本或回收站边界不符时失败。
func TestContractRecordDeleteHTTPMySQL(t *testing.T) {
	router, _, _, c := recordHTTPFixture(t)
	path := "/api/v1/contracts/" + c.ID
	var preview contract.RecordPreview
	if code := recordHTTPRequest(t, router, "GET", path+"/deletion-preview", "", &preview); code != 200 || !preview.CanDelete {
		t.Fatalf("preview status=%d value=%+v", code, preview)
	}
	var result contract.RecordMutation
	body := `{"expected_version":1,"reason":"合成误建"}`
	if code := recordHTTPRequest(t, router, "DELETE", path, body, &result); code != 200 || !result.Changed || result.Version != 2 {
		t.Fatalf("delete status=%d result=%+v", code, result)
	}
	if code := recordHTTPRequest(t, router, "GET", path, "", nil); code != 404 {
		t.Fatalf("deleted detail status=%d", code)
	}
	var page contract.DeletedRecordPage
	if code := recordHTTPRequest(t, router, "GET", "/api/v1/contracts?record_state=deleted&page=1&page_size=20", "", &page); code != 200 || page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("trash status=%d page=%+v", code, page)
	}
	if code := recordHTTPRequest(t, router, "DELETE", path, body, &result); code != 200 || result.Changed || result.Version != 2 {
		t.Fatalf("idempotent status=%d result=%+v", code, result)
	}
}

// TestContractRecordRestoreHTTPMySQL 用途：验证一份已删除草稿由恢复路由恢复原状态。
// 参数：t 为隔离 MySQL 最小场景，删除仅用于构造准备记录。
// 返回值：无；恢复回执或正常详情读取不符时失败。
func TestContractRecordRestoreHTTPMySQL(t *testing.T) {
	router, service, actor, c := recordHTTPFixture(t)
	if _, err := service.DeleteContract(context.Background(), actor, c.ID, 1, "恢复场景准备"); err != nil {
		t.Fatal(err)
	}
	var result contract.RecordMutation
	path := "/api/v1/contracts/" + c.ID
	if code := recordHTTPRequest(t, router, "POST", path+"/restore", `{"expected_version":2}`, &result); code != 200 || !result.Changed || result.Version != 3 || result.RecordState != contract.RecordActive {
		t.Fatalf("restore status=%d result=%+v", code, result)
	}
	var current contract.Contract
	if code := recordHTTPRequest(t, router, "GET", path, "", &current); code != 200 || current.Status != contract.StatusDraft {
		t.Fatalf("restored detail status=%d state=%s", code, current.Status)
	}
}
