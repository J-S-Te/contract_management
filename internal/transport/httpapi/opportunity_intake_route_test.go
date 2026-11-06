package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

type intakeRouteIdentity struct{}

func (intakeRouteIdentity) Authenticate(context.Context, *http.Request) (application.Principal, error) {
	return application.Principal{}, nil
}

func TestCMROpportunityIntakeRouteIsRegisteredAtWorkerPath(t *testing.T) {
	router := NewRouterWithAllIntegrations(nil, intakeRouteIdentity{}, nil, nil, nil, &CRMSignedCountIntegrationOptions{
		OpportunityIntakeEnabled: true, OpportunityIntakeRequireBearer: true,
	}, nil, nil)
	found := false
	for _, route := range router.Routes() {
		if route.Method == http.MethodPost && route.Path == "/internal/opportunity-signed-events" {
			found = true
		}
	}
	if !found {
		t.Fatalf("opportunity intake route missing: %+v", router.Routes())
	}
}

type intakeOnlyRepository struct {
	application.Repository
	accepted *application.OpportunityIntake
}

func (repository *intakeOnlyRepository) AcceptOpportunityIntake(_ context.Context, in application.OpportunityIntake) (application.OpportunityIntake, error) {
	repository.accepted = &in
	in.Status = application.OpportunityIntakeAccepted
	return in, nil
}
func (*intakeOnlyRepository) ListOpportunityIntakes(context.Context, string, string, string, int) (application.OpportunityIntakePage, error) {
	return application.OpportunityIntakePage{}, nil
}
func (*intakeOnlyRepository) GetOpportunityIntake(context.Context, string, string) (application.OpportunityIntake, error) {
	return application.OpportunityIntake{}, nil
}
func (*intakeOnlyRepository) ReviewOpportunityIntake(context.Context, string, string, string, application.OpportunityIntakeReview, string, string, string) (application.OpportunityIntake, error) {
	return application.OpportunityIntake{}, nil
}

func TestCMROpportunityIntakeRouteAcceptsMachineEnvelope(t *testing.T) {
	repo := &intakeOnlyRepository{}
	service := &application.Service{Repo: repo}
	router := NewRouterWithAllIntegrations(service, intakeRouteIdentity{}, nil, nil, nil, &CRMSignedCountIntegrationOptions{
		OpportunityIntakeEnabled: true, OpportunityIntakeRequireBearer: true, OpportunityIntakeBearerVerifier: stubVerifier{tenantID: "tenant-1"},
	}, nil, nil)
	body := `{"event_id":"evt-1","opportunity_id":7,"event_version":1,"opportunity_no":"SJ-7","customer_id":9,"contract_ref":"HT-7","expected_amount":"100.00","occurred_at":"2026-10-18T12:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/opportunity-signed-events", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"status":"ACCEPTED"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repo.accepted == nil || repo.accepted.TenantID != "tenant-1" || repo.accepted.EventID != "evt-1" {
		t.Fatalf("accepted=%+v", repo.accepted)
	}
}

// postOpportunityIntake 通过真实路由提交一次机器商机事件，返回响应记录器。
// 参数：router为已装配机器验收的API路由，body为请求JSON。
// 返回值：响应记录器，供调用方断言状态码与错误码。
func postOpportunityIntake(router http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/internal/opportunity-signed-events", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// newOpportunityIntakeRouter 装配仅在商机接入启用时的机器路由。
// 参数：service为合同服务，tenant为已验签令牌绑定租户。
// 返回值：可直接调用的API路由。
func newOpportunityIntakeRouter(service *application.Service, tenant string) http.Handler {
	return NewRouterWithAllIntegrations(service, intakeRouteIdentity{}, nil, nil, nil, &CRMSignedCountIntegrationOptions{
		OpportunityIntakeEnabled: true, OpportunityIntakeRequireBearer: true, OpportunityIntakeBearerVerifier: stubVerifier{tenantID: tenant},
	}, nil, nil)
}

// TestOpportunityIntakeTenantBoundToToken 验证租户只来自机器令牌：空租户与不一致租户都是403，
// 一致的显式 tenant_id 仍被接受，请求体不能扩大或改写租户边界。
// 参数：t为测试上下文。
// 返回值：无；越界租户被接受或合法请求被拒时失败。
func TestOpportunityIntakeTenantBoundToToken(t *testing.T) {
	body := `{"event_id":"evt-1","opportunity_id":7,"event_version":1,"opportunity_no":"SJ-7","customer_id":9,"contract_ref":"HT-7","expected_amount":"100.00","occurred_at":"2026-10-18T12:00:00Z"}`

	t.Run("mismatched body tenant", func(t *testing.T) {
		repo := &intakeOnlyRepository{}
		recorder := postOpportunityIntake(newOpportunityIntakeRouter(&application.Service{Repo: repo}, "tenant-1"),
			`{"tenant_id":"tenant-2",`+strings.TrimPrefix(body, "{"))
		if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "CON_INTAKE_TENANT_MISMATCH") {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if repo.accepted != nil {
			t.Fatalf("越租户请求到达服务层: %+v", repo.accepted)
		}
	})

	t.Run("matching body tenant", func(t *testing.T) {
		repo := &intakeOnlyRepository{}
		recorder := postOpportunityIntake(newOpportunityIntakeRouter(&application.Service{Repo: repo}, "tenant-1"),
			`{"tenant_id":"tenant-1",`+strings.TrimPrefix(body, "{"))
		if recorder.Code != http.StatusAccepted || repo.accepted == nil || repo.accepted.TenantID != "tenant-1" {
			t.Fatalf("status=%d body=%s accepted=%+v", recorder.Code, recorder.Body.String(), repo.accepted)
		}
	})
}

// TestOpportunityIntakeHandlerFailsClosedWithoutTokenTenant 直接驱动处理器，验证缺失令牌租户时
// 失败关闭；机器中间件本身也会401，这里是处理器层的纵深防御。
// 参数：t为测试上下文。
// 返回值：无；缺少租户仍继续处理请求时失败。
func TestOpportunityIntakeHandlerFailsClosedWithoutTokenTenant(t *testing.T) {
	repo := &intakeOnlyRepository{}
	handler := &Handler{service: &application.Service{Repo: repo}}
	writer := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(writer)
	request := httptest.NewRequest(http.MethodPost, "/internal/opportunity-signed-events", strings.NewReader(`{"event_id":"evt-1","opportunity_id":7,"event_version":1,"contract_ref":"HT-7","expected_amount":"100.00","occurred_at":"2026-10-18T12:00:00Z"}`))
	request.Header.Set("Content-Type", "application/json")
	c.Request = request
	c.Set(principalKey, application.Principal{TenantID: "", UserID: "data_analysis", Permissions: map[string]bool{"opportunity_intake.receive": true}})
	handler.acceptOpportunityIntake(c)
	if writer.Code != http.StatusForbidden || !strings.Contains(writer.Body.String(), "CON_INTAKE_TENANT_REQUIRED") {
		t.Fatalf("status=%d body=%s", writer.Code, writer.Body.String())
	}
	if repo.accepted != nil {
		t.Fatalf("缺少令牌租户仍到达服务层: %+v", repo.accepted)
	}
}

// TestOpportunityIntakeRejectsServerOwnedFieldsAndInvalidInput 验证请求面已收窄：
// status/version/reviewed_by/contract_id 等未知字段被严格解码拒绝，非法金额与超长标识为422，
// 合法请求仍保持202。
// 参数：t为测试上下文。
// 返回值：无；被收窄字段或非法输入仍受理时失败。
func TestOpportunityIntakeRejectsServerOwnedFieldsAndInvalidInput(t *testing.T) {
	base := map[string]any{
		"event_id": "evt-1", "opportunity_id": 7, "event_version": 1, "opportunity_no": "SJ-7",
		"customer_id": 9, "contract_ref": "HT-7", "expected_amount": "100.00", "occurred_at": "2026-10-18T12:00:00Z",
	}
	cases := []struct {
		name  string
		patch map[string]any
	}{
		{"status", map[string]any{"status": "LINK_CONFIRMED"}},
		{"version", map[string]any{"version": 2}},
		{"reviewed_by", map[string]any{"reviewed_by": "attacker"}},
		{"contract_id", map[string]any{"contract_id": "contract-1"}},
		{"amount precision", map[string]any{"expected_amount": "100.001"}},
		{"amount text", map[string]any{"expected_amount": "一百"}},
		{"amount overflow", map[string]any{"expected_amount": strings.Repeat("9", 21)}},
		{"amount empty", map[string]any{"expected_amount": ""}},
		{"contract_ref length", map[string]any{"contract_ref": strings.Repeat("A", 129)}},
		{"opportunity_no length", map[string]any{"opportunity_no": strings.Repeat("A", 129)}},
		{"source_request_id length", map[string]any{"source_request_id": strings.Repeat("A", 129)}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			payload := make(map[string]any, len(base)+len(test.patch))
			for key, value := range base {
				payload[key] = value
			}
			for key, value := range test.patch {
				payload[key] = value
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			repo := &intakeOnlyRepository{}
			recorder := postOpportunityIntake(newOpportunityIntakeRouter(&application.Service{Repo: repo}, "tenant-1"), string(encoded))
			if recorder.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if repo.accepted != nil {
				t.Fatalf("非法请求到达服务层: %+v", repo.accepted)
			}
		})
	}

	legal, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	repo := &intakeOnlyRepository{}
	recorder := postOpportunityIntake(newOpportunityIntakeRouter(&application.Service{Repo: repo}, "tenant-1"), string(legal))
	if recorder.Code != http.StatusAccepted || repo.accepted == nil {
		t.Fatalf("合法请求被拒: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
