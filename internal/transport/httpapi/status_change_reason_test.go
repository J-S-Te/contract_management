package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// statusChangeHTTPRepository 只记录是否真的进入了合同读取，用于证明超长原因在服务层之前/入口处被拒绝。
type statusChangeHTTPRepository struct {
	application.Repository
	looked bool
}

func (r *statusChangeHTTPRepository) GetContract(_ context.Context, _, id string) (contract.Contract, error) {
	r.looked = true
	return contract.Contract{}, apperrors.ErrNotFound
}

// TestStatusChangeReasonLengthLimit 验证 status-changes 的变更原因按列宽（VARCHAR(1000)/rune）限长。
// 参数：t为HTTP层测试上下文。
// 返回值：无；超长原因未按 422 拒绝、或正好 1000 字符被误拒时失败。
func TestStatusChangeReasonLengthLimit(t *testing.T) {
	repository := &statusChangeHTTPRepository{}
	identity := identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return draftHTTPActor("sales"), nil
	})
	router := NewRouter(&application.Service{Repo: repository}, identity, nil)
	post := func(reason string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/contracts/C/status-changes",
			strings.NewReader(`{"version":1,"target_status":"`+string(contract.StatusCompleted)+`","reason":"`+reason+`"}`))
		request.Header.Set("Content-Type", "application/json")
		// 同源写保护要求携带 APP_PUBLIC_URL 解析出的 Origin（见本包 TestMain）。
		request.Header.Set("Origin", "http://example.com")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	oversized := post(strings.Repeat("原", application.MaxStatusChangeReasonRunes+1))
	if oversized.Code != http.StatusUnprocessableEntity || !strings.Contains(oversized.Body.String(), "CON_VALIDATION_ERROR") {
		t.Fatalf("超长原因未被拒绝 status=%d body=%s", oversized.Code, oversized.Body.String())
	}
	if repository.looked {
		t.Fatal("超长原因已进入合同读取")
	}

	// 正好等于列宽的原因必须通过长度门禁，随后因合同不存在返回 404（证明不是长度规则拒绝）。
	within := post(strings.Repeat("原", application.MaxStatusChangeReasonRunes))
	if within.Code != http.StatusNotFound || !strings.Contains(within.Body.String(), "CON_NOT_FOUND") {
		t.Fatalf("边界长度原因被误拒 status=%d body=%s", within.Code, within.Body.String())
	}
}
