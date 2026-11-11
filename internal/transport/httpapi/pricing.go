package httpapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// listStandardPrices 返回已授权的标准价分页。
// 参数：c为可信会话请求。
// 返回值：无；结果写入标准响应包络。
func (h *Handler) listStandardPrices(c *gin.Context) {
	offset, e1 := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, e2 := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if e1 != nil || e2 != nil {
		writeError(c, application.ErrValidation)
		return
	}
	rows, err := h.service.ListStandardPrices(c.Request.Context(), principal(c), c.Query("status"), offset, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, map[string]any{"items": rows, "offset": offset, "limit": limit, "has_more": len(rows) == limit})
}

// saveStandardPrice 将目录写入交由应用层验证权限、职责与版本。
// 参数：c为已认证的新增、草稿编辑、发布或停用请求。
// 返回值：无；成功返回服务端保存的目录版本。
func (h *Handler) saveStandardPrice(c *gin.Context) {
	var body struct {
		contract.StandardPrice
		ExpectedVersion uint64 `json:"expected_version"`
	}
	if !decode(c, &body) {
		return
	}
	body.StandardPrice.ID = c.Param("priceID")
	action := "SAVE"
	switch c.Param("action") {
	case "publish":
		action = "PUBLISH"
	case "disable":
		action = "DISABLE"
	case "":
	default:
		writeError(c, application.ErrValidation)
		return
	}
	saved, err := h.service.SaveStandardPrice(c.Request.Context(), principal(c), body.StandardPrice, body.ExpectedVersion, action)
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, saved)
}

// dashboardMetrics 输出完整只读机器指标，不回退为旧dashboard摘要。
// 参数：c为受机器认证和许可约束的请求。
// 返回值：无；失败关闭，成功按兼容来源包络输出。
func (h *Handler) dashboardMetrics(c *gin.Context) {
	out, err := h.service.DashboardMetrics(c.Request.Context(), principal(c))
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	writeData(c, http.StatusOK, out)
}

// submitPricingRevision 接收显式重选并发起原合同独立审批。
// 参数：c为可信合同编辑请求，含版本、原因、付款计划和新基准日。
// 返回值：无；成功返回可在原审批中心读取的审批身份。
func (h *Handler) submitPricingRevision(c *gin.Context) {
	var body struct {
		Version      uint64                 `json:"version"`
		AmountMinor  int64                  `json:"amount_minor"`
		Currency     string                 `json:"currency"`
		PaymentTerms []contract.PaymentTerm `json:"payment_terms"`
		Pricing      *contract.Pricing      `json:"pricing"`
		Reason       string                 `json:"reason"`
	}
	if !decode(c, &body) {
		return
	}
	result, err := h.service.SubmitPricingRevision(c.Request.Context(), principal(c), c.Param("contractID"), application.ContractFinancialChange{ExpectedVersion: body.Version, AmountMinor: body.AmountMinor, Currency: body.Currency, PaymentTerms: body.PaymentTerms, Pricing: body.Pricing, Reason: body.Reason})
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusAccepted, result)
}
