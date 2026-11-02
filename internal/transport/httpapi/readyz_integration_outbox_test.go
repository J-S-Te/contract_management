package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

// readyzOutboxRepositoryStub 只为 /readyz 死信计数提供仓储能力；
// counts/err 由各用例注入，未嵌在 Repository 的其它方法上。
type readyzOutboxRepositoryStub struct {
	application.Repository
	counts map[string]int64
	err    error
	calls  int
}

func (s *readyzOutboxRepositoryStub) CountDeadIntegrationOutboxEvents(context.Context) (map[string]int64, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.counts, nil
}

type readyzEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Status            string           `json:"status"`
		Audit             string           `json:"audit"`
		SigningPhoneKey   string           `json:"signing_phone_key"`
		IntegrationOutbox string           `json:"integration_outbox"`
		Dead              map[string]int64 `json:"integration_outbox_dead"`
		DeadTotal         int64            `json:"integration_outbox_dead_total"`
	} `json:"data"`
}

// readyzReadyEnv 让既有就绪条件（签署密钥、审计）保持就绪，
// 使断言只聚焦本次新增的集成 Outbox 计数。
func readyzReadyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SIGNING_PHONE_ENCRYPTION_KEY_BASE64", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("PLATFORM_AUDIT_REQUIRED", "false")
	gin.SetMode(gin.TestMode)
}

func serveReadyz(t *testing.T, service *application.Service) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	NewRouter(service, nil, nil).ServeHTTP(response, request)
	return response
}

// BIZ-XEVT2-03：四类集成边各 1 条 dead 时，readyz 必须暴露逐类计数与总数。
func TestReadinessReportsIntegrationOutboxDeadCounts(t *testing.T) {
	readyzReadyEnv(t)
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{
		application.IntegrationOutboxFinancial:      1,
		application.IntegrationOutboxResponsibility: 1,
		application.IntegrationOutboxProject:        1,
		application.IntegrationOutboxLink:           1,
	}}
	response := serveReadyz(t, &application.Service{Repo: repository})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var parsed readyzEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode readyz body: %v; body = %s", err, response.Body.String())
	}
	if parsed.Data.IntegrationOutbox != "ready" {
		t.Fatalf("integration_outbox = %q, want ready; body = %s", parsed.Data.IntegrationOutbox, response.Body.String())
	}
	for _, kind := range application.IntegrationOutboxKinds() {
		if got := parsed.Data.Dead[kind]; got != 1 {
			t.Fatalf("%s dead = %d, want 1; body = %s", kind, got, response.Body.String())
		}
	}
	if parsed.Data.DeadTotal != 4 {
		t.Fatalf("dead total = %d, want 4; body = %s", parsed.Data.DeadTotal, response.Body.String())
	}
	// 既有就绪字段必须保持原语义。
	if parsed.Data.SigningPhoneKey != "ready" || parsed.Data.Audit != "disabled" || parsed.Data.Status != "ready" {
		t.Fatalf("existing readiness fields changed: %+v", parsed.Data)
	}
}

// 无死信时 truthful 0：四类都报 0、总数为 0，但仍保持就绪。
func TestReadinessReportsZeroIntegrationOutboxDeadCounts(t *testing.T) {
	readyzReadyEnv(t)
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{}}
	response := serveReadyz(t, &application.Service{Repo: repository})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	var parsed readyzEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode readyz body: %v; body = %s", err, response.Body.String())
	}
	if len(parsed.Data.Dead) != len(application.IntegrationOutboxKinds()) {
		t.Fatalf("dead = %+v, want all %d kinds", parsed.Data.Dead, len(application.IntegrationOutboxKinds()))
	}
	for _, kind := range application.IntegrationOutboxKinds() {
		if got := parsed.Data.Dead[kind]; got != 0 {
			t.Fatalf("%s dead = %d, want 0", kind, got)
		}
	}
	if parsed.Data.DeadTotal != 0 {
		t.Fatalf("dead total = %d, want 0", parsed.Data.DeadTotal)
	}
}

// 查询失败不得伪造 0：readyz 必须失败关闭并标记 unavailable。
func TestReadinessFailsClosedWhenIntegrationOutboxCountQueryFails(t *testing.T) {
	readyzReadyEnv(t)
	repository := &readyzOutboxRepositoryStub{err: errors.New("mysql: connection refused")}
	response := serveReadyz(t, &application.Service{Repo: repository})

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	var parsed readyzEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode readyz body: %v; body = %s", err, response.Body.String())
	}
	if parsed.Message != "not_ready" || parsed.Data.Status != "not_ready" {
		t.Fatalf("message = %q status = %q, want not_ready; body = %s", parsed.Message, parsed.Data.Status, response.Body.String())
	}
	if parsed.Data.IntegrationOutbox != "unavailable" {
		t.Fatalf("integration_outbox = %q, want unavailable; body = %s", parsed.Data.IntegrationOutbox, response.Body.String())
	}
	if parsed.Data.Dead != nil || parsed.Data.DeadTotal != 0 || strings.Contains(response.Body.String(), "integration_outbox_dead") {
		t.Fatalf("query failure must not fabricate counts; body = %s", response.Body.String())
	}
	// 其它就绪条件不受影响，便于运维区分是密钥/审计问题还是 DB 问题。
	if parsed.Data.SigningPhoneKey != "ready" || parsed.Data.Audit != "disabled" {
		t.Fatalf("unrelated readiness fields changed: %+v", parsed.Data)
	}
}

// 未装配计数能力（含 service 为 nil 的既有测试形态）时标记 disabled，
// 不改变既有 200 语义，也不把"无此能力"伪装成 0 条死信。
func TestReadinessMarksIntegrationOutboxDisabledWithoutCounterCapability(t *testing.T) {
	readyzReadyEnv(t)
	for name, service := range map[string]*application.Service{
		"nil-service":      nil,
		"non-counter-repo": {Repo: &integrationOutboxRepositoryStub{}},
	} {
		response := serveReadyz(t, service)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d; body = %s", name, response.Code, http.StatusOK, response.Body.String())
		}
		var parsed readyzEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("%s decode readyz body: %v; body = %s", name, err, response.Body.String())
		}
		if parsed.Data.IntegrationOutbox != "disabled" {
			t.Fatalf("%s integration_outbox = %q, want disabled; body = %s", name, parsed.Data.IntegrationOutbox, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "integration_outbox_dead") {
			t.Fatalf("%s disabled state must not expose counts; body = %s", name, response.Body.String())
		}
	}
}
