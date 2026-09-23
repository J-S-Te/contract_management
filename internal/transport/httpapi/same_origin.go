package httpapi

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// requireSameOriginWrite 对 cookie 会话认证的 /api/v1 不安全方法执行 CSRF 防护，
// 语义对齐 platform middleware.RequireAllowedOriginForUnsafeMethods（失败关闭）。
//
// 安全理由（SEC-D11）：前端 X-CSRF-Token 恒为 "1"，没有会话熵，防护完全依赖
// 浏览器强制附带的 Origin 头：
//   - 允许源 = APP_PUBLIC_URL 与 APP_CORS_ALLOWED_ORIGINS（可选逗号分隔，供
//     vite dev 等额外入口补充）各自解析出的 scheme://host；两者都缺失/非法时
//     允许集为空 → 全部写请求被拒绝（失败关闭，而不是放行）；
//   - Origin 缺失、非法或跨源一律拒绝；Sec-Fetch-Site 不能用于放行缺失的
//     Origin，其 cross-site 值直接拒绝（纵深防御）；
//   - 机器调用走 /internal/v1 Bearer 边界，不经过本中间件。
func (h *Handler) requireSameOriginWrite() gin.HandlerFunc {
	allowed := allowedWriteOrigins(os.Getenv("APP_PUBLIC_URL"), os.Getenv("APP_CORS_ALLOWED_ORIGINS"))
	return func(c *gin.Context) {
		if !isUnsafeMethod(c.Request.Method) {
			c.Next()
			return
		}
		origin := normalizeRequestOrigin(c.GetHeader("Origin"))
		if origin == "" {
			c.Abort()
			writeEnvelopeError(c, http.StatusForbidden, "AUTH_ORIGIN_REJECTED", "写请求来源校验失败", nil)
			return
		}
		if _, ok := allowed[origin]; !ok {
			c.Abort()
			writeEnvelopeError(c, http.StatusForbidden, "AUTH_ORIGIN_REJECTED", "写请求来源校验失败", nil)
			return
		}
		if strings.EqualFold(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")), "cross-site") {
			c.Abort()
			writeEnvelopeError(c, http.StatusForbidden, "AUTH_ORIGIN_REJECTED", "写请求来源校验失败", nil)
			return
		}
		c.Next()
	}
}

// allowedWriteOrigins 汇总主配置与逗号分隔的附加允许源；非法值被忽略后
// 允许集可能为空，此时中间件会失败关闭地拒绝所有写请求。
func allowedWriteOrigins(primary, extra string) map[string]struct{} {
	allowed := make(map[string]struct{})
	candidates := []string{primary}
	for _, entry := range strings.Split(extra, ",") {
		candidates = append(candidates, entry)
	}
	for _, raw := range candidates {
		if normalized := normalizeConfigOrigin(raw); normalized != "" {
			allowed[normalized] = struct{}{}
		}
	}
	return allowed
}

// normalizeConfigOrigin 提取配置值（可含路径，如 APP_PUBLIC_URL=/contract_management/）
// 的 scheme://host 作为允许源；非 http(s) 或缺 host 视为非法。
func normalizeConfigOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

// normalizeRequestOrigin 校验并规范化请求的 Origin 头：必须是纯源形式
// （无路径/查询/片段/用户信息），否则返回空串（拒绝）。
func normalizeRequestOrigin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}
