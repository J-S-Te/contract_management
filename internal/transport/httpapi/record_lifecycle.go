package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// previewContractDeletion 用途：向管理员返回最新记录摘要及删除或恢复阻断原因。
// 参数：c 为已通过认证和许可中间件的请求上下文。
// 返回值：无；写入预检响应或标准错误信封。
func (h *Handler) previewContractDeletion(c *gin.Context) {
	result, err := h.service.PreviewContractDeletion(c.Request.Context(), principal(c), c.Param("contractID"))
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, result)
}

// changeContractRecord 用途：消费明确版本的删除或恢复请求，复用服务层授权。
// 参数：c 为请求上下文，restore 表示恢复原记录还是逻辑删除。
// 返回值：无；写入当前目标状态回执或可追踪错误。
func (h *Handler) changeContractRecord(c *gin.Context, restore bool) {
	actor := principal(c)
	// 先检查权限再解析内容，拒绝者不能借参数错误探测管理能力。
	if err := h.service.AuthorizeContractRecordManagement(actor); err != nil {
		writeError(c, err)
		return
	}
	var input struct {
		ExpectedVersion uint64 `json:"expected_version"`
		Reason          string `json:"reason"`
	}
	if !decode(c, &input) {
		return
	}
	var result any
	var err error
	if restore {
		result, err = h.service.RestoreContract(c.Request.Context(), actor, c.Param("contractID"), input.ExpectedVersion, input.Reason)
	} else {
		result, err = h.service.DeleteContract(c.Request.Context(), actor, c.Param("contractID"), input.ExpectedVersion, input.Reason)
	}
	if err != nil {
		writeError(c, err)
		return
	}
	writeData(c, http.StatusOK, result)
}

// deleteContract 用途：适配已有 DELETE 合同路由。
// 参数：c 为认证后请求。
// 返回值：无；响应由公共记录管理处理器写入。
func (h *Handler) deleteContract(c *gin.Context) { h.changeContractRecord(c, false) }

// restoreContract 用途：适配合同恢复路由。
// 参数：c 为认证后请求。
// 返回值：无；响应由公共记录管理处理器写入。
func (h *Handler) restoreContract(c *gin.Context) { h.changeContractRecord(c, true) }
