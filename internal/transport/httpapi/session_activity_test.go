package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

// 生产认证器必须同时满足两个可选能力接口；否则路由 type assertion 会静默降级为
// 503/缺失可选字段，这里用编译期断言把契约固定下来。
var (
	_ SessionActivity      = (*platform.OIDCAuthenticator)(nil)
	_ SessionIdleInspector = (*platform.OIDCAuthenticator)(nil)
)

// sessionActivityIdentity 模拟实现了活动续期与空闲快照能力的浏览器身份。
type sessionActivityIdentity struct {
	principal     application.Principal
	idleTimeout   time.Duration
	idleRemaining time.Duration
	recordErr     error
	idleErr       error
	recordCalls   int
	idleCalls     int
}

func (identity *sessionActivityIdentity) Authenticate(context.Context, *http.Request) (application.Principal, error) {
	return identity.principal, nil
}

func (identity *sessionActivityIdentity) RecordActivity(context.Context, *http.Request) (time.Duration, time.Duration, error) {
	identity.recordCalls++
	return identity.idleTimeout, identity.idleRemaining, identity.recordErr
}

func (identity *sessionActivityIdentity) SessionIdle(context.Context, *http.Request) (time.Duration, time.Duration, error) {
	identity.idleCalls++
	return identity.idleTimeout, identity.idleRemaining, identity.idleErr
}

func sessionActivityTestIdentity() *sessionActivityIdentity {
	return &sessionActivityIdentity{
		principal:   application.Principal{TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1", Roles: []string{"sales"}},
		idleTimeout: 30 * time.Minute, idleRemaining: 25 * time.Minute,
	}
}

func sessionActivityRequest(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Origin", "http://localhost:8081")
	return request
}

// responseData 从统一响应信封中取出 data 对象，避免在测试里重复写结构体标签。
func responseData(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, response.Body.String())
	}
	if envelope["code"] != "OK" {
		t.Fatalf("envelope code = %v, want OK; body = %s", envelope["code"], response.Body.String())
	}
	data, ok := envelope["data"].(map[string]any)
	if !ok {
		t.Fatalf("envelope data = %#v", envelope["data"])
	}
	return data
}

// TestAuthActivityReturnsIdleWindowEnvelope 验证设计契约：POST /api/v1/auth/activity 返回
// 空闲期限与剩余毫秒，且只有该接口推进会话语义（通过 RecordActivity 调用次数体现）。
func TestAuthActivityReturnsIdleWindowEnvelope(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	identity := sessionActivityTestIdentity()
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, sessionActivityRequest(http.MethodPost, "/api/v1/auth/activity"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	data := responseData(t, response)
	if data["idle_timeout_seconds"] != float64(1800) || data["idle_remaining_ms"] != float64(1500000) {
		t.Fatalf("data = %#v", data)
	}
	if identity.recordCalls != 1 {
		t.Fatalf("RecordActivity calls = %d, want 1", identity.recordCalls)
	}
}

// TestAuthActivityRequiresSameOriginWrite 验证活动接口沿用浏览器写请求的 CSRF 保护：
// 缺失或跨源 Origin 一律拒绝，且不会触达续期逻辑。
func TestAuthActivityRequiresSameOriginWrite(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	for _, tc := range []struct {
		name   string
		origin string
	}{
		{"缺失 Origin", ""},
		{"跨源 Origin", "https://evil.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := sessionActivityTestIdentity()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/activity", nil)
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			response := httptest.NewRecorder()
			NewRouter(nil, identity, nil).ServeHTTP(response, request)

			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", response.Code, response.Body.String())
			}
			if identity.recordCalls != 0 {
				t.Fatalf("rejected CSRF request recorded %d interactions, want 0", identity.recordCalls)
			}
		})
	}
}

// TestAuthActivityRejectsInvalidSession 验证失效会话（空闲/绝对到期或已撤销）返回 401，
// 不会通过活动接口复活。
func TestAuthActivityRejectsInvalidSession(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	identity := sessionActivityTestIdentity()
	identity.recordErr = platform.ErrUnauthenticated
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, sessionActivityRequest(http.MethodPost, "/api/v1/auth/activity"))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
	}
}

// TestAuthActivityUnsupportedIdentity 验证不支持活动续期的身份提供方显式失败，而不是静默 200。
func TestAuthActivityUnsupportedIdentity(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{TenantID: "tenant-1", UserID: "user-1"}, nil
	})
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, sessionActivityRequest(http.MethodPost, "/api/v1/auth/activity"))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
}

// TestAuthActivityBypassesBusinessLicenseGate 验证活动续期按基础服务放行：
// 许可证只拒绝业务写（MUTATE_BUSINESS）时也不能阻断在线续期。
func TestAuthActivityBypassesBusinessLicenseGate(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	service := &application.Service{LicenseGate: historyOnlyLicense{}}
	identity := sessionActivityTestIdentity()
	response := httptest.NewRecorder()

	NewRouter(service, identity, nil).ServeHTTP(response, sessionActivityRequest(http.MethodPost, "/api/v1/auth/activity"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
}

// TestAuthActivityIsNotAuditedWrite 验证活动探针不进入审计写门禁：
// 强制审计模式缺少 reporter 时，业务写会 503，而续期仍必须可用。
func TestAuthActivityIsNotAuditedWrite(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://localhost:8081/contract_management/")
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	identity := sessionActivityTestIdentity()
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, sessionActivityRequest(http.MethodPost, "/api/v1/auth/activity"))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
}

// TestSessionActivityUsesEssentialServiceLicense 固定许可类别与审计跳过规则，
// 防止活动接口被误当成业务写操作。
func TestSessionActivityUsesEssentialServiceLicense(t *testing.T) {
	if got := contractLicenseOperation(http.MethodPost, "/api/v1/auth/activity"); got != core.ESSENTIAL_SERVICE {
		t.Fatalf("license operation = %v, want ESSENTIAL_SERVICE", got)
	}
	if !skipAudit(http.MethodPost, "/api/v1/auth/activity") {
		t.Fatal("activity probe must not be audited as a business write")
	}
	if !skipAudit(http.MethodPost, "/contract_management/api/v1/auth/activity") {
		t.Fatal("activity probe must be skipped with or without APP_PATH_PREFIX")
	}
	if skipAudit(http.MethodPost, "/api/v1/contracts") {
		t.Fatal("business write must stay audited")
	}
}

// TestAuthMeIncludesIdleWindowFields 验证 /auth/me 追加可选空闲字段且既有字段不变。
func TestAuthMeIncludesIdleWindowFields(t *testing.T) {
	identity := sessionActivityTestIdentity()
	identity.principal.Permissions = map[string]bool{"contract.read": true}
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	data := responseData(t, response)
	if data["tenant_id"] != "tenant-1" {
		t.Fatalf("existing tenant_id changed: %#v", data)
	}
	permissions, _ := data["permissions"].([]any)
	roles, _ := data["roles"].([]any)
	if len(permissions) != 1 || len(roles) != 1 {
		t.Fatalf("existing permissions/roles changed: %#v", data)
	}
	if data["idle_timeout_seconds"] != float64(1800) || data["idle_remaining_ms"] != float64(1500000) {
		t.Fatalf("idle fields = %#v", data)
	}
}

// TestAuthMeOmitsIdleWindowWhenIdentityDoesNotReport 验证旧身份实现不返回新字段，
// 保持既有 /auth/me 响应兼容。
func TestAuthMeOmitsIdleWindowWhenIdentityDoesNotReport(t *testing.T) {
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{TenantID: "tenant-1", UserID: "user-1", IdentityID: "identity-1"}, nil
	})
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	data := responseData(t, response)
	for _, key := range []string{"idle_timeout_seconds", "idle_remaining_ms"} {
		if _, exists := data[key]; exists {
			t.Fatalf("/auth/me unexpectedly exposes %q for identity without idle support", key)
		}
	}
}

// TestAuthMeRejectsSessionThatExpiredDuringIdleSnapshot 验证认证通过后会话并发失效时
// /auth/me 返回 401，而不是继续下发权限快照。
func TestAuthMeRejectsSessionThatExpiredDuringIdleSnapshot(t *testing.T) {
	identity := sessionActivityTestIdentity()
	identity.idleErr = platform.ErrUnauthenticated
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", response.Code, response.Body.String())
	}
}

// TestAuthMeDegradesWhenIdleSnapshotFails 验证可选字段读取失败（非未认证）不改变既有契约，
// 仍返回 200 与原有字段。
func TestAuthMeDegradesWhenIdleSnapshotFails(t *testing.T) {
	identity := sessionActivityTestIdentity()
	identity.idleErr = errors.New("session store temporarily unavailable")
	response := httptest.NewRecorder()

	NewRouter(nil, identity, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want degraded 200; body = %s", response.Code, response.Body.String())
	}
	data := responseData(t, response)
	if _, exists := data["idle_timeout_seconds"]; exists {
		t.Fatal("failed idle snapshot must not emit idle fields")
	}
	if _, exists := data["tenant_id"]; !exists {
		t.Fatal("existing /auth/me fields must stay available")
	}
}
