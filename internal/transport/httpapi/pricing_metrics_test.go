package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
)

type pricingMetricsHTTPRepository struct {
	application.Repository
	tenant string
}

// DiscountMetrics 记录真实HTTP中间件传入的验签租户。
// 参数：ctx为请求，tenant为机器主体绑定租户。
// 返回值：结构完整的最小来源快照测试夹具。
func (r *pricingMetricsHTTPRepository) DiscountMetrics(_ context.Context, tenant string) (application.DiscountMetrics, error) {
	r.tenant = tenant
	return application.DiscountMetrics{SchemaVersion: 1, TenantID: tenant, SourceCode: "contract_management", SnapshotAt: "2026-11-11T00:00:00Z", Complete: true, Basis: "隔离HTTP契约夹具", Rows: []application.DiscountMetricRow{}}, nil
}

// TestDiscountMetricsRouteUsesVerifiedTenant 验证新机器路由复用认证并忽略调用方租户头。
// 参数：t为测试上下文；无外部网络或业务数据库。
// 返回值：无；路由、包络、认证或租户边界不符即失败。
func TestDiscountMetricsRouteUsesVerifiedTenant(t *testing.T) {
	repo := &pricingMetricsHTTPRepository{}
	router := NewRouter(&application.Service{Repo: repo}, nil, &DashboardIntegrationOptions{Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{tenantID: "signed-tenant"}})
	for _, authorized := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/internal/v1/dashboard/metrics", nil)
		request.Header.Set("X-Tenant-ID", "forged")
		request.Header.Set("X-DA-Tenant-ID", "forged")
		if authorized {
			request.Header.Set("Authorization", "Bearer isolated-test-token")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if !authorized {
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("无凭据status=%d", response.Code)
			}
			continue
		}
		if response.Code != http.StatusOK || repo.tenant != "signed-tenant" {
			t.Fatalf("status=%d tenant=%s", response.Code, repo.tenant)
		}
		var envelope struct {
			Code string                      `json:"code"`
			Data application.DiscountMetrics `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != "OK" || envelope.Data.TenantID != "signed-tenant" || envelope.Data.SchemaVersion != 1 || !envelope.Data.Complete || envelope.Data.Rows == nil {
			t.Fatalf("包络不符：%s", response.Body.String())
		}
	}
}
