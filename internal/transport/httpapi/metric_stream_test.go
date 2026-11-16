package httpapi

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/metricstream"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// StreamDiscountMetrics 为真实HTTP授权链提供空流仓储替身。
// 参数：ctx/tenant 为服务端绑定请求，output 为响应流。
// 返回值：完整空流或输出错误；不替代真实MySQL验收。
func (r *pricingMetricsHTTPRepository) StreamDiscountMetrics(ctx context.Context, tenant string, output io.Writer) error {
	meta, _ := r.DiscountMetrics(ctx, tenant)
	w, err := metricstream.New(output, meta)
	if err != nil {
		return err
	}
	return w.End("", "")
}

// TestContractMetricStreamMachineBoundary 验证新路径原机器身份及历史读取许可不变。
// 参数：t 为隔离HTTP测试上下文。
// 返回值：无；匿名放行、伪造租户或许可分类错误时失败。
func TestContractMetricStreamMachineBoundary(t *testing.T) {
	path := "/internal/v1/dashboard/metrics/stream"
	if contractLicenseOperation("GET", path) != core.READ_HISTORY {
		t.Fatal("license classification")
	}
	repo := &pricingMetricsHTTPRepository{}
	router := NewRouter(&application.Service{Repo: repo}, nil, &DashboardIntegrationOptions{Enabled: true, RequireBearer: true, BearerVerifier: stubVerifier{tenantID: "signed-tenant"}})
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest("GET", path+"?tenant_id=forged", nil)
		req.Header.Set("X-DA-Tenant-ID", "forged")
		if authorized {
			req.Header.Set("Authorization", "Bearer synthetic")
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		if !authorized {
			if out.Code != 401 {
				t.Fatalf("anonymous %d", out.Code)
			}
			continue
		}
		if out.Code != 200 || repo.tenant != "signed-tenant" || !strings.Contains(out.Body.String(), `"type":"end"`) {
			t.Fatalf("stream %d %s", out.Code, out.Body.String())
		}
	}
}
