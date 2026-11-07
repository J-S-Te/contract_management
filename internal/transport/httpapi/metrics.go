package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditoutbox"
)

// metricsContentType 是 Prometheus 文本暴露格式（version=0.0.4）的固定 Content-Type。
// 抓取端按此头选择解析器；改变它会让既有 scrape 配置失效。
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// integrationOutboxMetrics 以 Prometheus 文本格式暴露两类积压计数：
// 四类集成 Outbox 的死信数（contract_integration_outbox_dead*），
// 以及事务内审计 Outbox 的待投递/死信数（contract_audit_outbox_pending|dead，
// 后者见 auditOutboxReadiness）。
//
// 暴露面与安全边界：
//  1. 只注册在 /healthz、/readyz 同级的无鉴权基础设施路由上（见 router.go），
//     刻意不进入 /api/v1 组：该组带 authenticate/auditWrites/requireSameOriginWrite/
//     licenseOperation，而 Prometheus 抓取既没有用户会话，也没有许可证操作上下文。
//     生产环境必须由网络层限制（Ingress/NetworkPolicy 只允许集群内监控抓取），
//     不要把这个端点暴露到公网或共享入口域名。
//  2. 只输出聚合计数与固定 kind 标签，不含租户、合同、事件 id 或 payload，
//     保证抓取管道被采信也不会泄露业务数据。
//  3. 失败关闭：计数能力缺失（disabled）或查询失败（unavailable）一律返回 503，
//     且正文只写 # ERROR 注释、不写任何 metric 采样行。绝不能用 0 兜底——
//     那会让监控看到"0 条积压"而掩盖"已生效未送达"的真实故障。
//
// 计数来源、四类 kind 的固定补齐、2s 查询上限均复用 /readyz 的
// integrationOutboxReadiness，避免指标与探针语义漂移。
func (h *Handler) integrationOutboxMetrics(c *gin.Context) {
	state, counts, total := integrationOutboxReadiness(c.Request.Context(), h.service)
	c.Header("Content-Type", metricsContentType)
	// dead 计数是运维决策依据，不能让反向代理/抓取端拿到缓存的旧值。
	c.Header("Cache-Control", "no-store")
	if state != "ready" {
		reason := "count_query_failed"
		if state == "disabled" {
			reason = "counter_unsupported"
		}
		c.String(http.StatusServiceUnavailable,
			"# ERROR contract integration outbox dead counts unavailable (reason="+reason+"): no metric samples emitted\n")
		return
	}
	// B4：事务内审计 Outbox（con_audit_outbox）的积压与死信同样挂在这个已有端点上。
	// 审计发布器未配置时投递器不启动，此时没有"投递积压"这一事实可报，
	// 因此不输出审计采样行（disabled 由 /readyz 的 audit_outbox_dispatcher 表达）；
	// 查询失败按本端点既有的失败关闭风格返回 503，绝不用 0 掩盖。
	auditState, auditPending, auditDead := auditOutboxReadiness(c.Request.Context(), h.auditOutbox)
	if auditState == "unavailable" {
		c.String(http.StatusServiceUnavailable,
			"# ERROR contract audit outbox counts unavailable (reason=count_query_failed): no metric samples emitted\n")
		return
	}
	body := renderIntegrationOutboxMetrics(counts, total)
	if auditState == "ready" {
		body += renderAuditOutboxMetrics(auditPending, auditDead)
	}
	c.String(http.StatusOK, body)
}

// auditOutboxReadiness 汇总 con_audit_outbox 的待投递与终态失败计数。
// 参数：ctx 为抓取请求上下文；store 为审计 Outbox 仓储，未装配时为 nil。
// 返回值：state 为 ready/disabled/unavailable；两个计数仅在 ready 时有效。
// 取舍：单条分组查询 + 2s 上限，与 integrationOutboxReadiness 同一风格，
// 不做重查询也不缓存，避免指标与表内事实漂移。
func auditOutboxReadiness(ctx context.Context, store *auditoutbox.GORMStore) (string, int64, int64) {
	if store == nil {
		return "disabled", 0, 0
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	pending, dead, err := store.DeliveryStatusCounts(probeCtx)
	if err != nil {
		slog.ErrorContext(ctx, "audit outbox count query failed", "error", err)
		return "unavailable", 0, 0
	}
	return "ready", pending, dead
}

// renderAuditOutboxMetrics 渲染审计 Outbox 的 pending/dead gauge 文本。
// 参数：pending 为 pending/retry/processing 行数，dead 为 failed 行数。
// 返回值：可直接追加到 /metrics 正文的字符串，每个采样行以换行结尾。
func renderAuditOutboxMetrics(pending, dead int64) string {
	var body strings.Builder
	body.WriteString("# HELP contract_audit_outbox_pending Undelivered events in the transaction audit outbox (delivery_status pending/retry/processing).\n")
	body.WriteString("# TYPE contract_audit_outbox_pending gauge\n")
	body.WriteString("contract_audit_outbox_pending ")
	body.WriteString(strconv.FormatInt(pending, 10))
	body.WriteString("\n")
	body.WriteString("# HELP contract_audit_outbox_dead Dead letters in the transaction audit outbox (delivery_status failed).\n")
	body.WriteString("# TYPE contract_audit_outbox_dead gauge\n")
	body.WriteString("contract_audit_outbox_dead ")
	body.WriteString(strconv.FormatInt(dead, 10))
	body.WriteString("\n")
	return body.String()
}

// renderIntegrationOutboxMetrics 渲染四类 dead gauge 与 total gauge 文本。
// 参数：counts 为 kind→数量（缺失 kind 视为 0，与 /readyz 补齐规则一致），total 为四类合计。
// 返回值：可直接作为 text/plain 暴露正文的字符串，每个采样行以换行结尾。
func renderIntegrationOutboxMetrics(counts map[string]int64, total int64) string {
	var body strings.Builder
	body.WriteString("# HELP contract_integration_outbox_dead Dead letters in the contract integration outbox, by outbound edge.\n")
	body.WriteString("# TYPE contract_integration_outbox_dead gauge\n")
	for _, kind := range application.IntegrationOutboxKinds() {
		body.WriteString(`contract_integration_outbox_dead{kind="`)
		body.WriteString(kind)
		body.WriteString(`"} `)
		body.WriteString(strconv.FormatInt(counts[kind], 10))
		body.WriteString("\n")
	}
	body.WriteString("# HELP contract_integration_outbox_dead_total Dead letters across all contract integration outbox outbound edges.\n")
	body.WriteString("# TYPE contract_integration_outbox_dead_total gauge\n")
	body.WriteString("contract_integration_outbox_dead_total ")
	body.WriteString(strconv.FormatInt(total, 10))
	body.WriteString("\n")
	return body.String()
}
