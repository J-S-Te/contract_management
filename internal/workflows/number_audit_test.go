package workflows

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/j-s-te/contract-management/internal/migration"
	"github.com/j-s-te/contract-management/internal/temporalworker"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

// 本文件覆盖合同编号登记表漂移每日巡检的三层契约：
// ① workflow 发现漂移不失败、只返回摘要；巡检执行失败才失败；
// ② activity 把 AuditReport 的三类计数正确映射成结果摘要（含示例条数上限）；
// ③ 观测面：有漂移用 Warn 结构化字段、干净用 Info，gauge 按 kind 暴露三类计数。

type fakeNumberAuditor struct {
	report migration.AuditReport
	err    error
	calls  int
}

func (f *fakeNumberAuditor) AuditContractNumberRegistry(context.Context) (migration.AuditReport, error) {
	f.calls++
	return f.report, f.err
}

// driftAuditReport 构造一份同时命中三类问题的巡检报告。
func driftAuditReport() migration.AuditReport {
	return migration.AuditReport{
		Duplicates: []migration.DuplicateContractNumber{
			{Normalized: "HT-2026-0001", Numbers: []string{"HT-2026-0001", "ht-2026-0001"}, Count: 2, Tenants: []string{"tenant-a", "tenant-b"}},
			{Normalized: "HT-2026-0004", Numbers: []string{"HT-2026-0004"}, Count: 2, Tenants: []string{"tenant-a"}},
		},
		Unregistered:      []migration.UnregisteredContractNumber{{ContractNumber: "HT-2026-0002", TenantID: "tenant-a", ContractID: "contract-2"}},
		UnregisteredTotal: 4,
		Mismatched:        []migration.RegistryMismatch{{ContractNumber: "HT-2026-0003", ContractTenantID: "tenant-a", ContractID: "contract-3", RegistryTenantID: "tenant-b", RegistryContractID: "contract-3"}},
		MismatchedTotal:   2,
	}
}

func TestContractNumberAuditWorkflowReturnsSummaryInsteadOfFailing(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{NumberAuditor: &fakeNumberAuditor{report: driftAuditReport()},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})

	env.ExecuteWorkflow(ContractNumberAuditWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError(), "漂移是数据状态而非执行故障，周期巡检不得因此失败")
	var result NumberAuditResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, 2, result.Duplicates)
	require.Equal(t, 4, result.Unregistered)
	require.Equal(t, 2, result.Mismatches)
	require.Equal(t, 8, result.Total)
	require.Len(t, result.Samples, numberAuditSampleLimit)
}

func TestContractNumberAuditWorkflowFailsWhenAuditExecutionFails(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{NumberAuditor: &fakeNumberAuditor{err: errors.New("数据库不可用")},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})

	env.ExecuteWorkflow(ContractNumberAuditWorkflow)

	require.True(t, env.IsWorkflowCompleted())
	require.Error(t, env.GetWorkflowError(), "连不上库属于执行故障，workflow 必须失败并交由 Temporal 重试/暴露")
}

func TestAuditContractNumbersMapsReportCountsAndSamples(t *testing.T) {
	auditor := &fakeNumberAuditor{report: driftAuditReport()}
	activities := &Activities{NumberAuditor: auditor}
	result, err := activities.AuditContractNumbers(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, result.Duplicates)
	require.Equal(t, 4, result.Unregistered)
	require.Equal(t, 2, result.Mismatches)
	require.Equal(t, 8, result.Total)
	// 示例最多 3 条且只含编号：两条重复组编号 + 一条未登记编号。
	require.Equal(t, []string{"HT-2026-0001", "HT-2026-0004", "HT-2026-0002"}, result.Samples)
	require.Equal(t, 1, auditor.calls)
}

func TestAuditContractNumbersReturnsErrorWhenAuditFails(t *testing.T) {
	activities := &Activities{NumberAuditor: &fakeNumberAuditor{err: errors.New("连接被拒绝")}}
	_, err := activities.AuditContractNumbers(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "合同编号登记表巡检未能完成")
}

func TestAuditContractNumbersRejectsMissingAuditor(t *testing.T) {
	_, err := (&Activities{}).AuditContractNumbers(context.Background())
	require.Error(t, err, "未装配 NumberAuditor 时必须报错，不能静默当成零漂移")
}

func TestAuditContractNumbersLogsDriftAndCleanRun(t *testing.T) {
	t.Run("有漂移用 Warn 输出结构化计数", func(t *testing.T) {
		var logs bytes.Buffer
		activities := &Activities{NumberAuditor: &fakeNumberAuditor{report: driftAuditReport()},
			Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		_, err := activities.AuditContractNumbers(context.Background())
		require.NoError(t, err)
		output := logs.String()
		require.Contains(t, output, `"level":"WARN"`)
		require.Contains(t, output, `"duplicates":2`)
		require.Contains(t, output, `"unregistered":4`)
		require.Contains(t, output, `"mismatches":2`)
		require.Contains(t, output, `"total":8`)
		require.Contains(t, output, `"samples":["HT-2026-0001","HT-2026-0004","HT-2026-0002"]`)
		// 日志只允许编号示例，不得混入租户或合同主键等客户信息。
		require.NotContains(t, output, "tenant-a")
		require.NotContains(t, output, "contract-2")
	})

	t.Run("干净时只输出一行 Info", func(t *testing.T) {
		var logs bytes.Buffer
		activities := &Activities{NumberAuditor: &fakeNumberAuditor{}, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		result, err := activities.AuditContractNumbers(context.Background())
		require.NoError(t, err)
		require.Zero(t, result.Total)
		require.Empty(t, result.Samples)
		require.Contains(t, logs.String(), `"level":"INFO"`)
		require.Contains(t, logs.String(), `"total":0`)
	})
}

func TestAuditContractNumbersExposesDriftGauges(t *testing.T) {
	registry := temporalworker.NewMetricsRegistry()
	activities := &Activities{NumberAuditor: &fakeNumberAuditor{report: driftAuditReport()},
		Logger:             slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		NumberAuditMetrics: registry}
	_, err := activities.AuditContractNumbers(context.Background())
	require.NoError(t, err)

	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	body := response.Body.String()
	for _, want := range []string{
		`contract_number_registry_drift{kind="duplicates"} 2`,
		`contract_number_registry_drift{kind="unregistered"} 4`,
		`contract_number_registry_drift{kind="mismatches"} 2`,
	} {
		require.Contains(t, body, want)
	}
}

func TestAuditContractNumbersWorksWithoutMetrics(t *testing.T) {
	// 指标未接线（nil）时巡检必须照常工作，只靠日志观测。
	activities := &Activities{NumberAuditor: &fakeNumberAuditor{report: driftAuditReport()},
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	result, err := activities.AuditContractNumbers(context.Background())
	require.NoError(t, err)
	require.Equal(t, 8, result.Total)
}
