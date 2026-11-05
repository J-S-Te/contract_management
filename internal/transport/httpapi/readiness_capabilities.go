package httpapi

import "github.com/gin-gonic/gin"

const readinessCapabilitiesContextKey = "readiness_capabilities"

// AttachReadinessCapabilities 给路由引擎注入 C14 能力维度快照；/readyz 会把它
// 作为 capabilities 段返回。可选未启用只反映在 state=disabled，不影响整体就绪
// 判定。经 gin 中间件传递以避免给 newRouter 家族的六个包装函数增加贯穿参数。
// 参数：r 为路由引擎；capabilities 为能力名到 enabled/state 描述的映射。
// 返回值：无。
func AttachReadinessCapabilities(r *gin.Engine, capabilities map[string]any) {
	if r == nil || len(capabilities) == 0 {
		return
	}
	r.Use(func(c *gin.Context) {
		c.Set(readinessCapabilitiesContextKey, capabilities)
		c.Next()
	})
}

// CapabilityState 按 C14 口径渲染能力状态：disabled 属可选未购买/被停用，
// 不计入就绪降级；ready 表示配置校验通过后的运行态。
// 参数：enabled 为该能力开关的运行事实。
// 返回值：ready 或 disabled。
func CapabilityState(enabled bool) string {
	if enabled {
		return "ready"
	}
	return "disabled"
}
