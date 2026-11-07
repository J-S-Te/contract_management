package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditoutbox"
)

// auditOutboxEnvelope 只解析本次新增的审计 Outbox 可观测字段。
type auditOutboxEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Status                 string `json:"status"`
		Audit                  string `json:"audit"`
		AuditOutboxDispatcher  string `json:"audit_outbox_dispatcher"`
		AuditOutboxRequired    bool   `json:"audit_outbox_required"`
		IntegrationOutboxState string `json:"integration_outbox"`
	} `json:"data"`
}

// readyzAuditEnv 让签署密钥就绪，仅由用例决定审计是否必开。
func readyzAuditEnv(t *testing.T, auditRequired string) {
	t.Helper()
	t.Setenv("SIGNING_PHONE_ENCRYPTION_KEY_BASE64", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("PLATFORM_AUDIT_REQUIRED", auditRequired)
	gin.SetMode(gin.TestMode)
}

// 审计发布器未配置时投递器不启动：可选模式下仍 200，但必须显式报告
// audit_outbox_dispatcher=disabled 与 audit_outbox_required=false，不能再沉默。
func TestReadinessReportsAuditOutboxDispatcherDisabled(t *testing.T) {
	readyzAuditEnv(t, "false")
	response := serveReadyz(t, &application.Service{Repo: &integrationOutboxRepositoryStub{}})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	var parsed auditOutboxEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode readyz body: %v; body = %s", err, response.Body.String())
	}
	if parsed.Data.AuditOutboxDispatcher != "disabled" || parsed.Data.AuditOutboxRequired {
		t.Fatalf("audit outbox fields = %+v; body = %s", parsed.Data, response.Body.String())
	}
	// 既有就绪语义不变：可选未启用只报告为 disabled，不引入新的 503 或状态值。
	if parsed.Data.Status != "ready" || parsed.Message != "ready" || parsed.Data.Audit != "disabled" {
		t.Fatalf("existing readiness semantics changed: %+v", parsed.Data)
	}
}

// 审计必开但发布器缺失：保持既有 503 失败关闭，并同时暴露投递器未启动。
func TestReadinessFailsClosedWhenAuditOutboxRequiredButMissing(t *testing.T) {
	readyzAuditEnv(t, "true")
	response := serveReadyz(t, &application.Service{Repo: &integrationOutboxRepositoryStub{}})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	var parsed auditOutboxEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode readyz body: %v; body = %s", err, response.Body.String())
	}
	if parsed.Data.Status != "not_ready" || parsed.Data.AuditOutboxDispatcher != "disabled" || !parsed.Data.AuditOutboxRequired {
		t.Fatalf("audit outbox fields = %+v; body = %s", parsed.Data, response.Body.String())
	}
}

// 审计发布器已配置时投递器在跑，字段必须如实报告 running。
func TestReadinessReportsAuditOutboxDispatcherRunning(t *testing.T) {
	readyzAuditEnv(t, "false")
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	NewRouter(&application.Service{Repo: &integrationOutboxRepositoryStub{}}, nil, nil, &stubAuditReporter{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	var parsed auditOutboxEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode readyz body: %v; body = %s", err, response.Body.String())
	}
	if parsed.Data.AuditOutboxDispatcher != "running" || parsed.Data.AuditOutboxRequired || parsed.Data.Audit != "enabled" {
		t.Fatalf("audit outbox fields = %+v; body = %s", parsed.Data, response.Body.String())
	}
}

// 未装配审计 Outbox 仓储时不输出审计采样行：没有投递器就没有"积压"事实可报，
// 不能用 0 伪装成健康。
func TestMetricsOmitsAuditOutboxFamiliesWithoutStore(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{}}
	response := serveMetrics(t, &application.Service{Repo: repository})
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "contract_audit_outbox") {
		t.Fatalf("disabled audit outbox must not emit samples; body = %s", response.Body.String())
	}
}

// 审计 Outbox 计数查询失败按端点既有风格失败关闭：503 且不输出任何采样行。
func TestMetricsFailsClosedWhenAuditOutboxCountFails(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{}}
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	// 未连接数据库的仓储：计数查询必然失败。
	NewRouterWithAuditOutbox(&application.Service{Repo: repository}, nil, nil, nil, nil, nil, nil, nil, &auditoutbox.GORMStore{}).ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", response.Code, response.Body.String())
	}
	assertNoMetricSamples(t, response.Body.String())
	if !strings.Contains(response.Body.String(), "contract audit outbox counts unavailable") {
		t.Fatalf("body must explain the failure; body = %s", response.Body.String())
	}
}
