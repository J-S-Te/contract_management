package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

// TestMain 固定 CSRF 同源校验的允许源环境（t.Setenv 不适用于包级初始化）：
// 本包测试的写请求统一携带 Origin: http://example.com，与 APP_PUBLIC_URL 解析结果一致。
func TestMain(m *testing.M) {
	_ = os.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management/")
	_ = os.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	os.Exit(m.Run())
}

// stubAuditReporter 模拟平台审计 ingest 失败（凭据失效、网络故障等）。
type stubAuditReporter struct {
	err   error
	calls int
}

func (s *stubAuditReporter) Report(_ context.Context, _ platform.AuditEvent) error {
	s.calls++
	return s.err
}

// captureAuditLog 捕获包级 slog 输出，断言审计失败路径留下 error 日志。
func captureAuditLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buffer
}

func auditTestIdentity() Identity {
	// 故意不授予 opportunity_intake.receive：handler 若被执行会立即写 403，
	// 与强制审计模式的 503 拒绝形成可区分的断言。
	return identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{TenantID: "tenant-1", UserID: "user-1"}, nil
	})
}

// SEC-D4b：强制审计模式下 reporter 缺失时必须在执行业务 handler 之前拒绝写入，
// 而不是像原来那样直接放行（产生无审计的成功写入）。
func TestAuditWriteRejectedWhenReporterMissingUnderRequiredAudit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	logs := captureAuditLog(t)

	request := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.com")
	response := httptest.NewRecorder()

	// audit reporter 缺省（第 4 个参数为空），service 为 nil：handler 一旦执行必然是
	// 业务响应（403）或 panic（500），503 + AUDIT_UNAVAILABLE 只可能来自审计拒绝。
	NewRouter(nil, auditTestIdentity(), nil).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUDIT_UNAVAILABLE") {
		t.Fatalf("body = %s, want AUDIT_UNAVAILABLE", response.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "audit reporter unavailable, rejecting request") {
		t.Fatalf("missing reject log: %s", logged)
	}
	if !strings.Contains(logged, "POST /api/v1/opportunity-intakes") || !strings.Contains(logged, "request_id") {
		t.Fatalf("reject log must contain route and request id: %s", logged)
	}
}

// SEC-D4b：强制审计模式下 Report 失败必须拒绝请求（503），业务响应被丢弃，
// 且日志包含路由与请求 id；不能像原来那样静默返回业务成功/失败响应。
func TestAuditWriteRejectedWhenReportFailsUnderRequiredAudit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	logs := captureAuditLog(t)
	reporter := &stubAuditReporter{err: errors.New("platform audit ingest unavailable")}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.com")
	response := httptest.NewRecorder()

	NewRouter(nil, auditTestIdentity(), nil, reporter).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUDIT_WRITE_REJECTED") {
		t.Fatalf("body = %s, want AUDIT_WRITE_REJECTED", response.Body.String())
	}
	if reporter.calls != 1 {
		t.Fatalf("Report calls = %d, want 1", reporter.calls)
	}
	logged := logs.String()
	if !strings.Contains(logged, "report platform audit failed") {
		t.Fatalf("missing report failure log: %s", logged)
	}
	if !strings.Contains(logged, "POST /api/v1/opportunity-intakes") || !strings.Contains(logged, "request_id") {
		t.Fatalf("failure log must contain route and request id: %s", logged)
	}
}

// SEC-D4b：非强制模式下 Report 失败不改变业务结果，但必须留下含路由与请求 id
// 的 error 日志（原来错误被空白标识符丢弃，完全不可见）。
func TestAuditWriteFailureLoggedWhenAuditNotRequired(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	gin.SetMode(gin.TestMode)
	logs := captureAuditLog(t)
	reporter := &stubAuditReporter{err: errors.New("platform audit ingest unavailable")}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.com")
	response := httptest.NewRecorder()

	NewRouter(nil, auditTestIdentity(), nil, reporter).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (business outcome unchanged); body = %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "report platform audit failed") {
		t.Fatalf("missing report failure log: %s", logged)
	}
	if !strings.Contains(logged, "POST /api/v1/opportunity-intakes") || !strings.Contains(logged, "request_id") {
		t.Fatalf("failure log must contain route and request id: %s", logged)
	}
}

// SEC-D4b：强制审计模式下上报成功时，缓冲的业务响应必须原样提交，
// 证明响应缓冲没有改变正常请求的返回形态。
func TestAuditWriteSuccessFlushesBusinessResponseUnderRequiredAudit(t *testing.T) {
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "true")
	gin.SetMode(gin.TestMode)
	logs := captureAuditLog(t)
	reporter := &stubAuditReporter{}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/opportunity-intakes", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://example.com")
	response := httptest.NewRecorder()

	NewRouter(nil, auditTestIdentity(), nil, reporter).ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "AUTH_FORBIDDEN") {
		t.Fatalf("business body not flushed: %s", response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if reporter.calls != 1 {
		t.Fatalf("Report calls = %d, want 1", reporter.calls)
	}
	if strings.Contains(logs.String(), "report platform audit failed") {
		t.Fatalf("unexpected failure log: %s", logs.String())
	}
}
