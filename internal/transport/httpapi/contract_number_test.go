package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// contractNumberRuleHTTPRepository 是编号规则端点的伪造仓储；
// 仅实现规则与流水的读写，其余 Repository 方法由嵌入接口占位。
type contractNumberRuleHTTPRepository struct {
	application.Repository
	rule    contract.ContractNumberRule
	found   bool
	current uint64
	saved   []contract.ContractNumberRule
}

func (r *contractNumberRuleHTTPRepository) GetContractNumberRule(context.Context) (contract.ContractNumberRule, bool, error) {
	return r.rule, r.found, nil
}

func (r *contractNumberRuleHTTPRepository) SaveContractNumberRule(_ context.Context, rule contract.ContractNumberRule) error {
	r.rule, r.found = rule, true
	r.saved = append(r.saved, rule)
	return nil
}

func (r *contractNumberRuleHTTPRepository) ContractNumberSequenceValue(context.Context, string, int) (uint64, error) {
	return r.current, nil
}

// contractNumberRuleEnvelope 复刻统一响应信封，用于断言字段名与错误码。
type contractNumberRuleEnvelope struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    *struct {
		Prefix       string    `json:"prefix"`
		YearSegment  string    `json:"year_segment"`
		SeqWidth     int       `json:"seq_width"`
		Suffix       string    `json:"suffix"`
		Sample       string    `json:"sample"`
		SeqYear      int       `json:"seq_year"`
		CurrentValue uint64    `json:"current_value"`
		UpdatedBy    string    `json:"updated_by"`
		UpdatedAt    time.Time `json:"updated_at"`
	} `json:"data"`
}

func contractNumberRuleRouter(actor application.Principal, repository *contractNumberRuleHTTPRepository) http.Handler {
	return NewRouter(&application.Service{Repo: repository}, identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return actor, nil
	}), nil)
}

func decodeContractNumberRuleEnvelope(t *testing.T, body []byte) contractNumberRuleEnvelope {
	t.Helper()
	var envelope contractNumberRuleEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("解码响应失败: %v body=%s", err, body)
	}
	return envelope
}

func contractNumberRulePutRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPut, "/api/v1/contract-number-rule", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://contracts.example.test")
	return request
}

// TestContractNumberRuleHTTPRead 验证读取端点返回规则、示例单号与规则桶的全系统当年水位。
func TestContractNumberRuleHTTPRead(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "https://contracts.example.test")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())
	repository := &contractNumberRuleHTTPRepository{
		rule:    contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4, UpdatedBy: "admin"},
		found:   true,
		current: 12,
	}
	actor := application.Principal{TenantID: "tenant-1", UserID: "reader", Permissions: map[string]bool{"contract.read": true}}
	response := httptest.NewRecorder()
	contractNumberRuleRouter(actor, repository).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/contract-number-rule", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	envelope := decodeContractNumberRuleEnvelope(t, response.Body.Bytes())
	if envelope.Code != "OK" || envelope.Data == nil {
		t.Fatalf("envelope=%+v", envelope)
	}
	wantSample := fmt.Sprintf("GTSR-%02d0013", year%100)
	if envelope.Data.Prefix != "GTSR-" || envelope.Data.YearSegment != contract.NumberYearSegmentYY ||
		envelope.Data.SeqWidth != 4 || envelope.Data.Sample != wantSample ||
		envelope.Data.SeqYear != year || envelope.Data.CurrentValue != 12 || envelope.Data.UpdatedBy != "admin" {
		t.Fatalf("data=%+v want sample=%s", envelope.Data, wantSample)
	}
	if len(repository.saved) != 0 {
		t.Fatal("读取不得写入规则")
	}
}

// TestContractNumberRuleHTTPWrite 验证写入端点全量覆盖并返回同一结构。
func TestContractNumberRuleHTTPWrite(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "https://contracts.example.test")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	// 生产取号按北京日历日取年，断言必须同口径。
	year := businessclock.YearOf(time.Now())
	repository := &contractNumberRuleHTTPRepository{current: 0}
	actor := application.Principal{TenantID: "tenant-1", UserID: "manager", Permissions: map[string]bool{"contract.number_rule.manage": true}}
	response := httptest.NewRecorder()
	contractNumberRuleRouter(actor, repository).ServeHTTP(response, contractNumberRulePutRequest(
		`{"prefix":"GTSR-","year_segment":"YY","seq_width":4,"suffix":""}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	envelope := decodeContractNumberRuleEnvelope(t, response.Body.Bytes())
	if envelope.Code != "OK" || envelope.Data == nil {
		t.Fatalf("envelope=%+v", envelope)
	}
	wantSample := fmt.Sprintf("GTSR-%02d0001", year%100)
	if envelope.Data.Sample != wantSample || envelope.Data.UpdatedBy != "manager" {
		t.Fatalf("data=%+v want sample=%s", envelope.Data, wantSample)
	}
	if len(repository.saved) != 1 || repository.saved[0].Prefix != "GTSR-" ||
		repository.saved[0].YearSegment != contract.NumberYearSegmentYY || repository.saved[0].SeqWidth != 4 {
		t.Fatalf("saved=%+v", repository.saved)
	}
}

// TestContractNumberRuleHTTPRejections 验证权限、校验与解码失败的错误码。
func TestContractNumberRuleHTTPRejections(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "https://contracts.example.test")
	t.Setenv("APP_CORS_ALLOWED_ORIGINS", "")
	for _, testCase := range []struct {
		name        string
		method      string
		body        string
		permissions map[string]bool
		roles       []string
		omitOrigin  bool
		status      int
		code        string
		message     string
	}{
		{name: "读取缺权限", method: http.MethodGet, permissions: map[string]bool{}, status: http.StatusForbidden, code: "AUTH_FORBIDDEN"},
		{name: "写入缺权限", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"YY","seq_width":4,"suffix":""}`,
			permissions: map[string]bool{"contract.read": true}, status: http.StatusForbidden, code: "AUTH_FORBIDDEN"},
		// 编号规则是全系统单行配置：模板管理权限不再放行写入。
		{name: "仅有模板管理权限写入", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"YY","seq_width":4,"suffix":""}`,
			permissions: map[string]bool{"contract.template.manage": true}, status: http.StatusForbidden, code: "AUTH_FORBIDDEN"},
		// 合同 admin 角色旁路已去掉：租户合同管理员不得改全系统前缀。
		{name: "合同管理员角色写入", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"YY","seq_width":4,"suffix":""}`,
			permissions: map[string]bool{"contract.template.manage": true, "contract.read": true},
			roles:       []string{"admin"}, status: http.StatusForbidden, code: "AUTH_FORBIDDEN"},
		{name: "年份段非法", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"MM","seq_width":4,"suffix":""}`,
			permissions: map[string]bool{"contract.number_rule.manage": true}, status: http.StatusUnprocessableEntity, code: "CON_VALIDATION_ERROR", message: "年份段"},
		{name: "位宽为零", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"YY","seq_width":0,"suffix":""}`,
			permissions: map[string]bool{"contract.number_rule.manage": true}, status: http.StatusUnprocessableEntity, code: "CON_VALIDATION_ERROR", message: "流水位宽"},
		{name: "未知字段", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"YY","seq_width":4,"suffix":"","extra":1}`,
			permissions: map[string]bool{"contract.number_rule.manage": true}, status: http.StatusUnprocessableEntity, code: "CON_VALIDATION_ERROR"},
		{name: "缺少写请求来源", method: http.MethodPut, body: `{"prefix":"GTSR-","year_segment":"YY","seq_width":4,"suffix":""}`,
			permissions: map[string]bool{"contract.number_rule.manage": true}, omitOrigin: true, status: http.StatusForbidden, code: "AUTH_ORIGIN_REJECTED"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository := &contractNumberRuleHTTPRepository{}
			actor := application.Principal{TenantID: "tenant-1", UserID: "operator", Permissions: testCase.permissions, Roles: testCase.roles}
			var request *http.Request
			if testCase.method == http.MethodGet {
				request = httptest.NewRequest(http.MethodGet, "/api/v1/contract-number-rule", nil)
			} else {
				request = contractNumberRulePutRequest(testCase.body)
				if testCase.omitOrigin {
					request.Header.Del("Origin")
				}
			}
			response := httptest.NewRecorder()
			contractNumberRuleRouter(actor, repository).ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != testCase.status || !strings.Contains(body, testCase.code) {
				t.Fatalf("status=%d body=%s want status=%d code=%s", response.Code, body, testCase.status, testCase.code)
			}
			if testCase.message != "" && !strings.Contains(body, testCase.message) {
				t.Fatalf("body=%s want message %q", body, testCase.message)
			}
			if len(repository.saved) != 0 {
				t.Fatal("被拒绝的请求不得写入规则")
			}
		})
	}
}

// TestAuditResourceClassifiesNumberRulePath 验证审计资源归类，且不被
// contract-templates 旁路误命中。
func TestAuditResourceClassifiesNumberRulePath(t *testing.T) {
	for _, testCase := range []struct {
		path, resource string
	}{
		{path: "/api/v1/contract-number-rule", resource: "CONTRACT_NUMBER_RULE"},
		{path: "/api/v1/contract-templates", resource: "CONTRACT_TEMPLATE"},
	} {
		ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginContext.Request = httptest.NewRequest(http.MethodPut, testCase.path, bytes.NewReader(nil))
		resource, id := auditResource(ginContext)
		if resource != testCase.resource || id != "" {
			t.Fatalf("path=%s resource=%s id=%s want %s", testCase.path, resource, id, testCase.resource)
		}
	}
}
