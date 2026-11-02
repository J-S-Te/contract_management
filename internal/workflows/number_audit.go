package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/j-s-te/contract-management/internal/migration"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// 本文件是合同编号登记表漂移的每日只读巡检：复用合同 worker 已有的 Temporal Cron
// 注册通道，每天跑一次 migrations 的巡检口径，只记日志/指标，不主动通知任何人。
// 它与审批、发号主链路完全独立：只读查询、独立 workflow、不写业务库与审计 outbox。

const (
	// ContractNumberAuditWorkflowName 是每日巡检 workflow 的注册名，供 worker 与测试共用。
	ContractNumberAuditWorkflowName = "contract-number-audit-daily"
	// ActivityAuditContractNumbers 是只读巡检活动的注册名。
	ActivityAuditContractNumbers = "AuditContractNumbers"
	// numberAuditDriftGauge 是漂移数量指标名，按 kind 标签区分三类问题。
	// 用 gauge 而不是 counter：每天覆盖为当次巡检值，便于直接看“现在有多少漂移”。
	numberAuditDriftGauge = "contract_number_registry_drift"
	// numberAuditSampleLimit 限制结果与日志里携带的编号示例条数。
	numberAuditSampleLimit = 3
	// numberAuditActivityTimeout 是单次巡检的 StartToClose 上限。跨租户全表统计在
	// 大数据量下可能耗时较长，给到 5 分钟，避免活动被过早判定超时后反复重试。
	numberAuditActivityTimeout = 5 * time.Minute
)

// NumberAuditResult 是周期巡检的结果摘要：三类计数、总数与最多 3 条编号示例。
// 刻意不携带合同明细（客户名称、金额等），因为结果会进入 Temporal 历史与日志。
type NumberAuditResult struct {
	Duplicates   int      `json:"duplicates"`
	Unregistered int      `json:"unregistered"`
	Mismatches   int      `json:"mismatches"`
	Total        int      `json:"total"`
	Samples      []string `json:"samples,omitempty"`
}

// NumberAuditor 是只读巡检出口，由仓储实现（*mysql.Repository）。
// 独立于 ApprovalStore：周期巡检不该扩大审批写入契约的幂等要求。
type NumberAuditor interface {
	AuditContractNumberRegistry(context.Context) (migration.AuditReport, error)
}

// NumberAuditMetricsSink 是巡检指标的最小出口，*temporalworker.MetricsRegistry 直接满足；
// 未接入（nil）时只记日志，不为巡检引入新的指标框架。
type NumberAuditMetricsSink interface {
	WithTags(tags map[string]string) client.MetricsHandler
}

// ContractNumberAuditWorkflow 是每天执行一次的合同编号登记表只读巡检。
//
// 设计取舍：巡检发现漂移时 workflow 正常返回摘要，不返回错误。漂移是数据状态而不是
// 执行故障，把周期巡检打成失败只会污染 Temporal 失败视图、让告警噪声掩盖真正的执行
// 故障，而下一轮 cron 仍会面对同一份数据；观测依赖 Activity 输出的结构化日志与 gauge
// 指标。只有巡检本身执行失败（连不上库、活动重试耗尽）才让 workflow 失败。
func ContractNumberAuditWorkflow(ctx workflow.Context) (NumberAuditResult, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: numberAuditActivityTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 10 * time.Second, BackoffCoefficient: 2,
			MaximumInterval: 2 * time.Minute, MaximumAttempts: 5,
		},
	})
	var result NumberAuditResult
	if err := workflow.ExecuteActivity(actx, ActivityAuditContractNumbers).Get(ctx, &result); err != nil {
		return NumberAuditResult{}, err
	}
	return result, nil
}

// AuditContractNumbers 执行一次跨租户只读巡检，返回三类计数并写入日志与指标。
//
// 只读巡检刻意不参与商业许可证写权限门禁（checkLicense 检查的是 MUTATE_BUSINESS）：
// 许可证过期时运维仍然需要看到数据漂移，且巡检本身不产生任何业务写入。
func (a *Activities) AuditContractNumbers(ctx context.Context) (NumberAuditResult, error) {
	if a.NumberAuditor == nil {
		return NumberAuditResult{}, errors.New("合同编号巡检未接入仓储：worker 装配缺少 NumberAuditor")
	}
	report, err := a.NumberAuditor.AuditContractNumberRegistry(ctx)
	if err != nil {
		// 连不上库、登记表缺失等执行故障返回错误，交由 Temporal 按活动重试策略重试。
		return NumberAuditResult{}, fmt.Errorf("合同编号登记表巡检未能完成：%w", err)
	}
	result := numberAuditResultFromReport(report)
	a.logNumberAuditResult(result)
	a.recordNumberAuditMetrics(result)
	return result, nil
}

// numberAuditResultFromReport 把 migration.AuditReport 映射成工作流结果摘要。
//
// 参数：report 为巡检报告。
// 返回值：三类计数 + 总数 + 最多 3 条编号示例的结果。
func numberAuditResultFromReport(report migration.AuditReport) NumberAuditResult {
	result := NumberAuditResult{
		Duplicates:   len(report.Duplicates),
		Unregistered: report.UnregisteredTotal,
		Mismatches:   report.MismatchedTotal,
		Total:        report.Total(),
		Samples:      numberAuditSamples(report),
	}
	return result
}

// numberAuditSamples 提取最多 3 条编号示例：重复编号取归一后编号，未登记与登记错配
// 取合同行编号。只输出编号本身，不带租户、客户或金额等敏感信息。
//
// 参数：report 为巡检报告。
// 返回值：去重后的编号示例，最多 numberAuditSampleLimit 条。
func numberAuditSamples(report migration.AuditReport) []string {
	samples := make([]string, 0, numberAuditSampleLimit)
	seen := make(map[string]struct{}, numberAuditSampleLimit)
	add := func(number string) {
		if number == "" || len(samples) >= numberAuditSampleLimit {
			return
		}
		if _, exists := seen[number]; exists {
			return
		}
		seen[number] = struct{}{}
		samples = append(samples, number)
	}
	for _, group := range report.Duplicates {
		add(group.Normalized)
	}
	for _, detail := range report.Unregistered {
		add(detail.ContractNumber)
	}
	for _, detail := range report.Mismatched {
		add(detail.ContractNumber)
	}
	return samples
}

// logNumberAuditResult 输出巡检结论：有漂移用 Warn 带结构化计数，干净时 Info 一行。
// 日志只含计数与编号示例，不包含任何客户信息。
//
// 参数：result 为巡检结果摘要。
// 返回值：无。
func (a *Activities) logNumberAuditResult(result NumberAuditResult) {
	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if result.Total > 0 {
		logger.Warn("合同编号登记表漂移巡检发现差异",
			"duplicates", result.Duplicates, "unregistered", result.Unregistered,
			"mismatches", result.Mismatches, "total", result.Total, "samples", result.Samples)
		return
	}
	logger.Info("合同编号登记表巡检通过",
		"duplicates", 0, "unregistered", 0, "mismatches", 0, "total", 0)
}

// recordNumberAuditMetrics 把三类漂移计数写入 worker 私有指标注册表（如已接入）。
//
// 指标形态为 contract_number_registry_drift{kind="duplicates|unregistered|mismatches"}，
// 由 TEMPORAL_METRICS_ADDRESS 暴露的 /metrics 抓取。gauge 名不带 _total 后缀：
// Prometheus 里 _total 是 counter 语义，这里是每日覆盖的瞬时值。
//
// 参数：result 为巡检结果摘要。
// 返回值：无。
func (a *Activities) recordNumberAuditMetrics(result NumberAuditResult) {
	if a.NumberAuditMetrics == nil {
		return
	}
	for _, item := range []struct {
		kind  string
		value int
	}{
		{"duplicates", result.Duplicates},
		{"unregistered", result.Unregistered},
		{"mismatches", result.Mismatches},
	} {
		a.NumberAuditMetrics.WithTags(map[string]string{"kind": item.kind}).
			Gauge(numberAuditDriftGauge).Update(float64(item.value))
	}
}
