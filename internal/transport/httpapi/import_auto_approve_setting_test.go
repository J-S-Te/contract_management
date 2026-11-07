package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// importAutoApproveHTTPStore 是运行开关 HTTP 契约测试的仓储替身：
// 只实现租户设置读写，其余 Repository 方法由内嵌接口兜底（用例不触达）。
type importAutoApproveHTTPStore struct {
	application.Repository
	setting     contract.ImportAutoApproveSetting
	getErr      error
	setErr      error
	setCalls    int
	lastTenant  string
	lastEnabled bool
	lastVersion uint64
	lastActor   string
}

// GetImportAutoApproveSetting 返回预置设置或错误。
// 参数：ctx 为上下文，tenantID 为租户。
// 返回值：预置设置。
func (r *importAutoApproveHTTPStore) GetImportAutoApproveSetting(_ context.Context, tenantID string) (contract.ImportAutoApproveSetting, error) {
	r.lastTenant = tenantID
	if r.getErr != nil {
		return contract.ImportAutoApproveSetting{}, r.getErr
	}
	return r.setting, nil
}

// SetImportAutoApproveSetting 记录写入方式并推进版本。
// 参数：ctx 为上下文，tenantID/enabled/expectedVersion/actorID 为写入契约。
// 返回值：预置错误或 version+1 后的设置。
func (r *importAutoApproveHTTPStore) SetImportAutoApproveSetting(_ context.Context, tenantID string, enabled bool, expectedVersion uint64, actorID string) (contract.ImportAutoApproveSetting, error) {
	r.setCalls++
	r.lastTenant, r.lastEnabled, r.lastVersion, r.lastActor = tenantID, enabled, expectedVersion, actorID
	if r.setErr != nil {
		return contract.ImportAutoApproveSetting{}, r.setErr
	}
	return contract.ImportAutoApproveSetting{TenantID: tenantID, Enabled: enabled, Version: expectedVersion + 1, UpdatedBy: actorID}, nil
}

// importAutoApproveEnvelope 是开关接口的响应信封（含 data 的宽松结构）。
type importAutoApproveEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    *struct {
		CapabilityEnabled bool    `json:"capability_enabled"`
		Enabled           bool    `json:"enabled"`
		Version           uint64  `json:"version"`
		UpdatedBy         string  `json:"updated_by"`
		UpdatedAt         *string `json:"updated_at"`
	} `json:"data"`
}

// newImportAutoApproveRouter 建立只含运行开关接口的路由与管理员身份。
// 参数：t 为测试上下文，store 为仓储替身，capability 为 env 能力开关。
// 返回值：路由与可注入的主体。
func newImportAutoApproveRouter(t *testing.T, store *importAutoApproveHTTPStore, capability bool, actor application.Principal) http.Handler {
	t.Helper()
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	t.Setenv("APP_PUBLIC_URL", "http://example.com")
	service := &application.Service{Repo: store, ContractImportAutoApproveHistorical: capability}
	return NewRouter(service, identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return actor, nil
	}), nil)
}

// decodeImportAutoApproveEnvelope 解析响应信封。
// 参数：t 为测试上下文，response 为记录器。
// 返回值：信封。
func decodeImportAutoApproveEnvelope(t *testing.T, response *httptest.ResponseRecorder) importAutoApproveEnvelope {
	t.Helper()
	var body importAutoApproveEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
	return body
}

// TestGetImportAutoApproveSettingAPIContract 验证 GET 契约：
// 非 admin 403；admin 读到 capability_enabled 与租户设置；无记录时 version=0、
// updated_by 为空、updated_at 为 null。
// 参数：t 为测试上下文。
// 返回值：状态码或 data 形状不符时失败。
func TestGetImportAutoApproveSettingAPIContract(t *testing.T) {
	admin := application.Principal{TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"}}
	nonAdmin := application.Principal{TenantID: "tenant-1", UserID: "user-1", Roles: []string{"contract_specialist"}}

	t.Run("non-admin-is-forbidden", func(t *testing.T) {
		router := newImportAutoApproveRouter(t, &importAutoApproveHTTPStore{}, true, nonAdmin)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-import/auto-approve", nil))
		body := decodeImportAutoApproveEnvelope(t, response)
		if response.Code != 403 || body.Code != "AUTH_FORBIDDEN" {
			t.Fatalf("status=%d body=%+v", response.Code, body)
		}
	})

	t.Run("capability-and-zero-setting", func(t *testing.T) {
		router := newImportAutoApproveRouter(t, &importAutoApproveHTTPStore{}, true, admin)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-import/auto-approve", nil))
		body := decodeImportAutoApproveEnvelope(t, response)
		if response.Code != 200 || body.Code != "OK" || body.Message != "操作成功" || body.Data == nil {
			t.Fatalf("status=%d body=%+v", response.Code, body)
		}
		if !body.Data.CapabilityEnabled || body.Data.Enabled || body.Data.Version != 0 ||
			body.Data.UpdatedBy != "" || body.Data.UpdatedAt != nil {
			t.Fatalf("data=%+v", *body.Data)
		}
	})

	t.Run("capability-off-reported", func(t *testing.T) {
		router := newImportAutoApproveRouter(t, &importAutoApproveHTTPStore{}, false, admin)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-import/auto-approve", nil))
		body := decodeImportAutoApproveEnvelope(t, response)
		if response.Code != 200 || body.Data == nil || body.Data.CapabilityEnabled {
			t.Fatalf("status=%d body=%+v", response.Code, body)
		}
	})
}

// TestPutImportAutoApproveSettingAPIGuards 验证 PUT 的门禁顺序与错误码：
// 能力关闭 → 422 IMPORT_AUTO_APPROVE_DISABLED；版本不符 → 409 IMPORT_REVISION_CONFLICT；
// 参数缺失/负数/未知字段 → 422 CON_VALIDATION_ERROR；且被拒绝时不写入。
// 参数：t 为测试上下文。
// 返回值：状态码、错误码或副作用不符时失败。
func TestPutImportAutoApproveSettingAPIGuards(t *testing.T) {
	admin := application.Principal{TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"}}
	put := func(t *testing.T, store *importAutoApproveHTTPStore, capability bool, body string) (*httptest.ResponseRecorder, importAutoApproveEnvelope) {
		t.Helper()
		router := newImportAutoApproveRouter(t, store, capability, admin)
		request := httptest.NewRequest(http.MethodPut, "http://example.com/api/v1/contract-import/auto-approve", strings.NewReader(body))
		request.Header.Set("Origin", "http://example.com")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response, decodeImportAutoApproveEnvelope(t, response)
	}

	t.Run("capability-disabled", func(t *testing.T) {
		store := &importAutoApproveHTTPStore{}
		response, body := put(t, store, false, `{"enabled":true,"expected_version":0}`)
		if response.Code != 422 || body.Code != "IMPORT_AUTO_APPROVE_DISABLED" || store.setCalls != 0 {
			t.Fatalf("status=%d body=%+v setCalls=%d", response.Code, body, store.setCalls)
		}
	})

	t.Run("version-conflict", func(t *testing.T) {
		store := &importAutoApproveHTTPStore{setErr: application.ErrImportConflict}
		response, body := put(t, store, true, `{"enabled":true,"expected_version":0}`)
		if response.Code != 409 || body.Code != "IMPORT_REVISION_CONFLICT" {
			t.Fatalf("status=%d body=%+v", response.Code, body)
		}
	})

	for _, invalid := range []string{
		`{"expected_version":0}`,
		`{"enabled":true}`,
		`{"enabled":"true","expected_version":0}`,
		`{"enabled":true,"expected_version":-1}`,
		`{"enabled":true,"expected_version":0,"unknown":1}`,
	} {
		t.Run("invalid-"+invalid, func(t *testing.T) {
			store := &importAutoApproveHTTPStore{}
			response, body := put(t, store, true, invalid)
			if response.Code != 422 || body.Code != "CON_VALIDATION_ERROR" || store.setCalls != 0 {
				t.Fatalf("status=%d body=%+v setCalls=%d", response.Code, body, store.setCalls)
			}
		})
	}
}

// TestPutImportAutoApproveSettingAPISuccess 验证成功路径：
// 写入使用已认证主体的租户与用户，返回与 GET 相同的结构且 version 已 +1。
// 参数：t 为测试上下文。
// 返回值：转发参数或响应形状不符时失败。
func TestPutImportAutoApproveSettingAPISuccess(t *testing.T) {
	admin := application.Principal{TenantID: "tenant-1", UserID: "admin-1", Roles: []string{"admin"}}
	store := &importAutoApproveHTTPStore{}
	router := newImportAutoApproveRouter(t, store, true, admin)
	request := httptest.NewRequest(http.MethodPut, "http://example.com/api/v1/contract-import/auto-approve",
		strings.NewReader(`{"enabled":true,"expected_version":0}`))
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	body := decodeImportAutoApproveEnvelope(t, response)
	if response.Code != 200 || body.Code != "OK" || body.Data == nil {
		t.Fatalf("status=%d body=%+v", response.Code, body)
	}
	if store.setCalls != 1 || store.lastTenant != "tenant-1" || !store.lastEnabled || store.lastVersion != 0 || store.lastActor != "admin-1" {
		t.Fatalf("calls=%d tenant=%q enabled=%t expected=%d actor=%q",
			store.setCalls, store.lastTenant, store.lastEnabled, store.lastVersion, store.lastActor)
	}
	if !body.Data.CapabilityEnabled || !body.Data.Enabled || body.Data.Version != 1 || body.Data.UpdatedBy != "admin-1" {
		t.Fatalf("data=%+v", *body.Data)
	}
}
