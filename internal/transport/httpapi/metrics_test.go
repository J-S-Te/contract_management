package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

const metricsExpectedContentType = "text/plain; version=0.0.4; charset=utf-8"

func serveMetrics(t *testing.T, service *application.Service) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	response := httptest.NewRecorder()
	// identity 传 nil：本端点必须挂在无鉴权的基础设施路由上，
	// 若被 authenticate() 包住，nil identity 会直接 panic/500。
	NewRouter(service, nil, nil).ServeHTTP(response, request)
	return response
}

// metricsSamples 解析 Prometheus 文本正文的采样行，返回 "name{labels}" → 值。
// 解析失败（无空格分隔）时 fail，避免测试放过格式错误。
func metricsSamples(t *testing.T, body string) map[string]float64 {
	t.Helper()
	samples := map[string]float64{}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		index := strings.LastIndex(line, " ")
		if index <= 0 {
			t.Fatalf("malformed metric line %q; body = %s", line, body)
		}
		value, err := strconv.ParseFloat(line[index+1:], 64)
		if err != nil {
			t.Fatalf("malformed metric value in %q: %v; body = %s", line, err, body)
		}
		samples[line[:index]] = value
	}
	return samples
}

// assertNoMetricSamples 断言失败关闭响应里没有任何 gauge 采样行（即没有伪造 0）。
func assertNoMetricSamples(t *testing.T, body string) {
	t.Helper()
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		t.Fatalf("fail-closed response must not emit metric samples, got %q; body = %s", line, body)
	}
}

// BIZ-XEVT2-03：四类集成边各 1 条 dead 时 /metrics 必须暴露四个 gauge=1 与 total=4。
func TestMetricsExposesIntegrationOutboxDeadGauges(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{
		application.IntegrationOutboxFinancial:      1,
		application.IntegrationOutboxResponsibility: 1,
		application.IntegrationOutboxProject:        1,
		application.IntegrationOutboxLink:           1,
	}}
	response := serveMetrics(t, &application.Service{Repo: repository})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != metricsExpectedContentType {
		t.Fatalf("Content-Type = %q, want %q", got, metricsExpectedContentType)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"# HELP contract_integration_outbox_dead ",
		"# TYPE contract_integration_outbox_dead gauge",
		"# HELP contract_integration_outbox_dead_total ",
		"# TYPE contract_integration_outbox_dead_total gauge",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body missing %q; body = %s", expected, body)
		}
	}
	samples := metricsSamples(t, body)
	if len(samples) != len(application.IntegrationOutboxKinds())+1 {
		t.Fatalf("samples = %+v, want %d kind gauges plus total; body = %s",
			samples, len(application.IntegrationOutboxKinds()), body)
	}
	for _, kind := range application.IntegrationOutboxKinds() {
		key := `contract_integration_outbox_dead{kind="` + kind + `"}`
		if got, ok := samples[key]; !ok || got != 1 {
			t.Fatalf("%s = %v (present=%v), want 1; body = %s", key, got, ok, body)
		}
	}
	if got := samples["contract_integration_outbox_dead_total"]; got != 4 {
		t.Fatalf("dead total = %v, want 4; body = %s", got, body)
	}
}

// 无死信时 truthful 0：四类都报 0、总数为 0，不是把缺失当成"没数据"。
func TestMetricsReportsZeroIntegrationOutboxDeadCounts(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{}}
	response := serveMetrics(t, &application.Service{Repo: repository})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	samples := metricsSamples(t, response.Body.String())
	for _, kind := range application.IntegrationOutboxKinds() {
		key := `contract_integration_outbox_dead{kind="` + kind + `"}`
		if got, ok := samples[key]; !ok || got != 0 {
			t.Fatalf("%s = %v (present=%v), want 0; body = %s", key, got, ok, response.Body.String())
		}
	}
	if got := samples["contract_integration_outbox_dead_total"]; got != 0 {
		t.Fatalf("dead total = %v, want 0; body = %s", got, response.Body.String())
	}
}

// 仓储只返回部分 kind 时按 0 补齐，total 只统计真实计数（与 /readyz 一致）。
func TestMetricsFillsMissingKindsWithZero(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{
		application.IntegrationOutboxProject: 3,
	}}
	response := serveMetrics(t, &application.Service{Repo: repository})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	samples := metricsSamples(t, response.Body.String())
	for _, kind := range application.IntegrationOutboxKinds() {
		key := `contract_integration_outbox_dead{kind="` + kind + `"}`
		want := 0.0
		if kind == application.IntegrationOutboxProject {
			want = 3
		}
		if got, ok := samples[key]; !ok || got != want {
			t.Fatalf("%s = %v (present=%v), want %v; body = %s", key, got, ok, want, response.Body.String())
		}
	}
	if got := samples["contract_integration_outbox_dead_total"]; got != 3 {
		t.Fatalf("dead total = %v, want 3; body = %s", got, response.Body.String())
	}
}

// 查询失败不得伪造 0：/metrics 失败关闭 503，正文只有 # ERROR 注释。
func TestMetricsFailsClosedWhenIntegrationOutboxCountQueryFails(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{err: errors.New("mysql: connection refused")}
	response := serveMetrics(t, &application.Service{Repo: repository})

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != metricsExpectedContentType {
		t.Fatalf("Content-Type = %q, want %q", got, metricsExpectedContentType)
	}
	body := response.Body.String()
	assertNoMetricSamples(t, body)
	if !strings.Contains(body, "# ERROR") || !strings.Contains(body, "reason=count_query_failed") {
		t.Fatalf("body must explain the failure with an # ERROR comment; body = %s", body)
	}
	// 原始数据库错误可能包含内部信息，不应回显到无鉴权端点。
	if strings.Contains(body, "connection refused") {
		t.Fatalf("body must not leak raw query error; body = %s", body)
	}
}

// 未装配计数能力（service 为 nil 或仓储未实现计数接口）时同样失败关闭 503，
// 不把"无此能力"伪装成 0 条死信，也不返回 200。
func TestMetricsFailsClosedWithoutCounterCapability(t *testing.T) {
	for name, service := range map[string]*application.Service{
		"nil-service":      nil,
		"non-counter-repo": {Repo: &integrationOutboxRepositoryStub{}},
	} {
		response := serveMetrics(t, service)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want %d; body = %s", name, response.Code, http.StatusServiceUnavailable, response.Body.String())
		}
		body := response.Body.String()
		assertNoMetricSamples(t, body)
		if !strings.Contains(body, "reason=counter_unsupported") {
			t.Fatalf("%s body must mark counter_unsupported; body = %s", name, body)
		}
	}
}

// 端点位于无鉴权的基础设施路由：不带会话/cookie 也能抓取，
// 且只暴露聚合计数，不含租户、合同或事件 id 等业务字段。
func TestMetricsRouteIsUnauthenticatedAndAggregateOnly(t *testing.T) {
	repository := &readyzOutboxRepositoryStub{counts: map[string]int64{
		application.IntegrationOutboxFinancial: 2,
	}}
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Cookie", "")
	response := httptest.NewRecorder()
	gin.SetMode(gin.TestMode)
	NewRouter(&application.Service{Repo: repository}, nil, nil).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{"tenant", "contract_id", "aggregate_id", "payload", "last_error", "principal"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("metrics body must not expose %q; body = %s", forbidden, body)
		}
	}
	// 只应包含两个指标名与固定 kind 标签。
	if strings.Count(body, "contract_integration_outbox_dead") < len(application.IntegrationOutboxKinds())+2 {
		t.Fatalf("body missing expected metric families; body = %s", body)
	}
}
