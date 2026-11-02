package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// changeContractFinancialTerms 是受控“合同财务变更”入口（BIZ-XEVT2-05 合同侧）。
//
// 该路由只把请求转成应用层用例；全部门禁都在应用层与仓储事务内：
// 开关 CONTRACT_FINANCIAL_CHANGE_ENABLED、contract.edit 权限与数据范围、
// 合同已审批且无在途审批、失败关闭的计划/金额校验、审计与按版本财务事件。
// 这里刻意不复制门禁逻辑，避免出现“HTTP 层放行、事务层才拒绝”的分叉。
func (h *Handler) changeContractFinancialTerms(c *gin.Context) {
	var body struct {
		Version      uint64                 `json:"version"`
		AmountMinor  int64                  `json:"amount_minor"`
		Currency     string                 `json:"currency"`
		PaymentTerms []contract.PaymentTerm `json:"payment_terms"`
		Reason       string                 `json:"reason"`
	}
	if !decode(c, &body) {
		return
	}
	updated, err := h.service.ChangeContractFinancialTerms(c.Request.Context(), principal(c), c.Param("contractID"), application.ContractFinancialChange{
		ExpectedVersion: body.Version,
		AmountMinor:     body.AmountMinor,
		Currency:        body.Currency,
		PaymentTerms:    body.PaymentTerms,
		Reason:          body.Reason,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]any{
		"contract_id":     updated.ID,
		"contract_number": updated.Number,
		"status":          updated.Status,
		"version":         updated.Version,
		"amount_minor":    updated.AmountMinor,
		"currency":        updated.Currency,
		"payment_terms":   updated.PaymentTerms,
		"updated_at":      updated.UpdatedAt,
	})
}
