package httpapi

import (
	"github.com/gin-gonic/gin"
)

// dashboardMetricsStream 在原机器鉴权和许可边界输出完整分块快照。
// 参数：c 为已验证租户的只读请求。
// 返回值：无；写出后失败通过缺end拒绝发布，写出前使用原错误响应。
func (h *Handler) dashboardMetricsStream(c *gin.Context) {
	c.Header("Content-Type", "application/x-ndjson")
	c.Header("Cache-Control", "no-store")
	if err := h.service.StreamDashboardMetrics(c.Request.Context(), principal(c), c.Writer); err != nil && !c.Writer.Written() {
		writeError(c, err)
	}
}
