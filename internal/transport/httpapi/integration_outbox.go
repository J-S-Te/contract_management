package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

// listFailedIntegrationEvents 列出当前租户四类集成 Outbox 的 dead 行。
// 参数：kind 必填（financial/responsibility/project/link），limit 为可选上限。
// 返回值：200 列表；未知 kind 返回明确校验错误，权限不足返回 403。
func (h *Handler) listFailedIntegrationEvents(c *gin.Context) {
	if !principal(c).Has(application.IntegrationOutboxManagerPermission) {
		writeEnvelopeError(c, http.StatusForbidden, "CON_FORBIDDEN", "当前用户没有执行此操作的权限", nil)
		return
	}
	kind := c.Query("kind")
	if !application.ValidIntegrationOutboxKind(kind) {
		writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "kind 必须是 financial、responsibility、project 或 link", nil)
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limit <= 0 {
		limit = 100
	}
	items, err := h.service.ListFailedIntegrationEvents(c.Request.Context(), principal(c), kind, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	response := make([]map[string]any, 0, len(items))
	for _, item := range items {
		response = append(response, map[string]any{
			"id": item.ID, "kind": item.Kind, "aggregate_id": item.AggregateID,
			"event": item.Event, "attempts": item.Attempts, "last_error": item.LastError,
			"created_at": item.CreatedAt.Format(time.RFC3339),
			"updated_at": item.UpdatedAt.Format(time.RFC3339),
		})
	}
	writeData(c, http.StatusOK, map[string]any{"items": response})
}

// retryFailedIntegrationEvent 把一条 dead 行重置为可重试并写审计。
// 参数：kind/id 为路径参数，只接受当前租户的死信。
// 返回值：200 表示已重置；未知 kind 返回校验错误；不存在返回 404。
func (h *Handler) retryFailedIntegrationEvent(c *gin.Context) {
	if !principal(c).Has(application.IntegrationOutboxManagerPermission) {
		writeEnvelopeError(c, http.StatusForbidden, "CON_FORBIDDEN", "当前用户没有执行此操作的权限", nil)
		return
	}
	kind, id := c.Param("kind"), c.Param("id")
	found, err := h.service.RetryFailedIntegrationEvent(c.Request.Context(), principal(c), kind, id)
	if err != nil {
		if errors.Is(err, application.ErrValidation) {
			writeEnvelopeError(c, http.StatusUnprocessableEntity, "CON_VALIDATION_ERROR", "kind 必须是 financial、responsibility、project 或 link", nil)
			return
		}
		writeError(c, err)
		return
	}
	if !found {
		writeEnvelopeError(c, http.StatusNotFound, "CON_NOT_FOUND", "未找到可重试的集成投递事件", nil)
		return
	}
	writeData(c, http.StatusOK, map[string]string{"id": id, "kind": kind, "status": "retry"})
}
