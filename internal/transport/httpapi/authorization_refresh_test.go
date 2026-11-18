package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

// TestAuthMePreservesAuthorizationFailureStatus 验证授权拒绝与身份依赖故障不被当作重新登录。
// 参数：t 为会话自描述 HTTP 测试上下文。
// 返回值：无；拒绝未返回 403 或依赖故障未返回 503 时失败。
func TestAuthMePreservesAuthorizationFailureStatus(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{platform.ErrAuthorizationForbidden, 403}, {platform.ErrAuthorizationServiceUnavailable, 503}, {platform.ErrUnauthenticated, 401}} {
		identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
			return application.Principal{}, tc.err
		})
		response := httptest.NewRecorder()
		NewRouter(nil, identity, nil).ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/auth/me", nil))
		if response.Code != tc.status {
			t.Fatalf("err=%v status=%d want=%d", tc.err, response.Code, tc.status)
		}
	}
}
