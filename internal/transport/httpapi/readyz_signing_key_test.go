package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// SEC-D7：签署人手机号列加密密钥必填——/readyz 在密钥缺失时失败关闭，
// 在密钥就绪（且审计不强制缺配）时返回 200 并暴露 signing_phone_key 状态。
func TestReadinessFailsClosedWhenSigningPhoneKeyMissing(t *testing.T) {
	t.Setenv("SIGNING_PHONE_ENCRYPTION_KEY_BASE64", "")
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	NewRouter(nil, nil, nil).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "\"signing_phone_key\":\"missing\"") {
		t.Fatalf("body = %s, want signing_phone_key=missing", response.Body.String())
	}
}

func TestReadinessReportsSigningPhoneKeyReady(t *testing.T) {
	t.Setenv("SIGNING_PHONE_ENCRYPTION_KEY_BASE64", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()

	NewRouter(nil, nil, nil).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "\"signing_phone_key\":\"ready\"") {
		t.Fatalf("body = %s, want signing_phone_key=ready", response.Body.String())
	}
}
