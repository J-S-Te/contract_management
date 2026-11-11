package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// changeContractFinancialTerms 接收保留冻结标准基准的受控金额变更。
// 参数：c为可信请求，包含并发版本、金额、计划及原因。
// 返回值：无；成功返回当前有效版本，错误写入统一包络。
func (h *Handler) changeContractFinancialTerms(c *gin.Context) {
	var body struct {
		Pricing      *contract.Pricing      `json:"pricing,omitempty"`
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
		Pricing:         body.Pricing,
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
		"pricing":         updated.Pricing,
		"contract_number": updated.Number,
		"status":          updated.Status,
		"version":         updated.Version,
		"amount_minor":    updated.AmountMinor,
		"currency":        updated.Currency,
		"payment_terms":   updated.PaymentTerms,
		"updated_at":      updated.UpdatedAt,
	})
}
