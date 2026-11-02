package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// financialChangeHandlerRepository 是路由测试用的受控变更仓储桩。
type financialChangeHandlerRepository struct {
	application.Repository
	contract contract.Contract
	change   *application.ContractFinancialChange
	tenant   string
	actor    string
	err      error
	result   contract.Contract
}

func (r *financialChangeHandlerRepository) GetContract(context.Context, string, string) (contract.Contract, error) {
	return r.contract, nil
}

func (r *financialChangeHandlerRepository) ApplyContractFinancialChange(_ context.Context, tenantID string, change application.ContractFinancialChange, actor string) (contract.Contract, error) {
	r.tenant, r.actor = tenantID, actor
	r.change = &change
	return r.result, r.err
}

func financialChangeHandlerFixture() (*Handler, *financialChangeHandlerRepository) {
	active := contract.Contract{
		ID: "C1", TenantID: "tenant", Number: "HT-1", Status: contract.StatusActive, Version: 7,
		AmountMinor: 10000, Currency: "CNY", OwnerIdentityID: "identity",
		PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "100.00"}},
	}
	repo := &financialChangeHandlerRepository{contract: active, result: active}
	repo.result.AmountMinor, repo.result.Version = 20000, 8
	repo.result.PaymentTerms = []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-20", Amount: "200.00"}}
	service := &application.Service{Repo: repo, ContractFinancialChangeEnabled: true}
	return &Handler{service: service}, repo
}

func financialChangeRequest(t *testing.T, payload string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/contracts/C1/financial-terms", strings.NewReader(payload))
	c.Params = gin.Params{{Key: "contractID", Value: "C1"}}
	c.Set(principalKey, application.Principal{
		TenantID: "tenant", UserID: "actor", IdentityID: "identity",
		Permissions:      map[string]bool{"contract.edit": true},
		PermissionScopes: map[string]contract.ScopeFilter{"contract.edit": {AllowAll: true}},
	})
	return c, w
}

const validFinancialChangePayload = `{"version":7,"amount_minor":20000,"currency":"CNY","payment_terms":[{"installment_no":1,"due_date":"2026-12-20","amount":"200.00"}],"reason":"客户追加范围"}`

// TestFinancialChangeRouteRejectsWhenSwitchClosed 验证默认关闭时路由返回明确
// 403 且不触碰仓储：入口开关是第一道失败关闭门禁。
func TestFinancialChangeRouteRejectsWhenSwitchClosed(t *testing.T) {
	h, repo := financialChangeHandlerFixture()
	h.service.ContractFinancialChangeEnabled = false
	c, w := financialChangeRequest(t, validFinancialChangePayload)
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "CON_FINANCIAL_CHANGE_DISABLED") {
		t.Fatalf("开关关闭未拒绝：%d %s", w.Code, w.Body.String())
	}
	if repo.change != nil {
		t.Fatal("开关关闭时不得调用仓储")
	}
}

// TestFinancialChangeRouteRequiresPermissionAndApproval 验证权限与审批前置
// 在 HTTP 层按既有信封映射为 403/409。
func TestFinancialChangeRouteRequiresPermissionAndApproval(t *testing.T) {
	h, _ := financialChangeHandlerFixture()
	c, w := financialChangeRequest(t, validFinancialChangePayload)
	c.Set(principalKey, application.Principal{TenantID: "tenant", UserID: "actor"})
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "AUTH_FORBIDDEN") {
		t.Fatalf("缺少权限未拒绝：%d %s", w.Code, w.Body.String())
	}

	h, repo := financialChangeHandlerFixture()
	repo.contract.Status = contract.StatusDraft
	c, w = financialChangeRequest(t, validFinancialChangePayload)
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "CON_STATE_CONFLICT") {
		t.Fatalf("未审批合同未拒绝：%d %s", w.Code, w.Body.String())
	}

	h, repo = financialChangeHandlerFixture()
	repo.err = application.ErrFinancialChangeApprovalRequired
	c, w = financialChangeRequest(t, validFinancialChangePayload)
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "CON_FINANCIAL_CHANGE_APPROVAL_REQUIRED") {
		t.Fatalf("审批证据缺失未拒绝：%d %s", w.Code, w.Body.String())
	}

	h, repo = financialChangeHandlerFixture()
	repo.err = errors.New("storage unavailable")
	c, w = financialChangeRequest(t, validFinancialChangePayload)
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("存储失败必须 5xx：%d %s", w.Code, w.Body.String())
	}
}

// TestFinancialChangeRouteReturnsVersionedSnapshot 验证成功响应暴露版本、
// 金额与替换后的付款计划，供前端核对版本跃迁。
func TestFinancialChangeRouteReturnsVersionedSnapshot(t *testing.T) {
	h, repo := financialChangeHandlerFixture()
	c, w := financialChangeRequest(t, validFinancialChangePayload)
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusOK {
		t.Fatalf("成功变更被拒：%d %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Data struct {
			ContractID   string                 `json:"contract_id"`
			Version      uint64                 `json:"version"`
			AmountMinor  int64                  `json:"amount_minor"`
			PaymentTerms []contract.PaymentTerm `json:"payment_terms"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.ContractID != "C1" || envelope.Data.Version != 8 || envelope.Data.AmountMinor != 20000 || len(envelope.Data.PaymentTerms) != 1 || envelope.Data.PaymentTerms[0].Amount != "200.00" {
		t.Fatalf("响应未反映版本跃迁：%+v", envelope.Data)
	}
	if repo.change == nil || repo.change.ExpectedVersion != 7 || repo.tenant != "tenant" || repo.actor != "actor" {
		t.Fatalf("请求未按可信身份与并发版本传递：%+v", repo.change)
	}
}

// TestFinancialChangeRouteRejectsCurrencyChange 验证币种变更按 422 拒绝，
// 不会进入仓储事务。
func TestFinancialChangeRouteRejectsCurrencyChange(t *testing.T) {
	h, repo := financialChangeHandlerFixture()
	c, w := financialChangeRequest(t, `{"version":7,"amount_minor":20000,"currency":"USD","payment_terms":[{"installment_no":1,"due_date":"2026-12-20","amount":"200.00"}],"reason":"换币种"}`)
	h.changeContractFinancialTerms(c)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "CON_FINANCIAL_CHANGE_CURRENCY_LOCKED") {
		t.Fatalf("币种变更未拒绝：%d %s", w.Code, w.Body.String())
	}
	if repo.change != nil {
		t.Fatal("币种变更不得调用仓储")
	}
}
