package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// getContractNumberRule 返回系统级编号规则与当前规则桶当年全系统流水水位。
// 参数：c 为 HTTP 上下文。
// 返回值：无；权限或依赖失败写入统一错误信封。
func (h *Handler) getContractNumberRule(c *gin.Context) {
	view, err := h.service.GetContractNumberRule(c.Request.Context(), principal(c))
	if err != nil {
		writeContractNumberRuleError(c, err)
		return
	}
	writeData(c, http.StatusOK, view)
}

// updateContractNumberRule 全量覆盖系统级编号规则；仅影响后续取号。
// 参数：c 为 HTTP 上下文。
// 返回值：无；校验失败返回 422 与中文原因。
func (h *Handler) updateContractNumberRule(c *gin.Context) {
	var body struct {
		Prefix      string `json:"prefix"`
		YearSegment string `json:"year_segment"`
		SeqWidth    int    `json:"seq_width"`
		Suffix      string `json:"suffix"`
	}
	if !decode(c, &body) {
		return
	}
	view, err := h.service.UpdateContractNumberRule(c.Request.Context(), principal(c), contract.ContractNumberRule{
		Prefix: body.Prefix, YearSegment: body.YearSegment, SeqWidth: body.SeqWidth, Suffix: body.Suffix,
	})
	if err != nil {
		writeContractNumberRuleError(c, err)
		return
	}
	writeData(c, http.StatusOK, view)
}

// writeContractNumberRuleError 保留规则校验的中文原因，不把可修正配置报成 5xx。
// 参数：c 为 HTTP 上下文，err 为应用层错误。
// 返回值：无。
func writeContractNumberRuleError(c *gin.Context, err error) {
	if errors.Is(err, application.ErrValidation) {
		detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), application.ErrValidation.Error()+":"))
		if detail == "" {
			detail = "合同编号规则不合法"
		}
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", detail, nil)
		return
	}
	writeError(c, err)
}
