package migration

import (
	"fmt"
	"strings"
	"testing"
)

// conflictGroup 构造一组重号，供错误信息与合并逻辑用例复用。
func conflictGroup(normalized string, numbers []string, count int, tenants ...string) DuplicateContractNumber {
	return DuplicateContractNumber{Normalized: normalized, Numbers: numbers, Count: count, Tenants: tenants}
}

// TestContractNumberConflictErrorReportsGroups 验证冲突报告的中文口径：
// 组数、迁移已中止、逐条编号与租户、处理指引。
// 参数：t 为测试上下文。
// 返回值：无。
func TestContractNumberConflictErrorReportsGroups(t *testing.T) {
	err := &ContractNumberConflictError{Groups: []DuplicateContractNumber{
		conflictGroup("GTSR-260001", []string{"GTSR-260001"}, 3, "01JAAA", "01JBBB"),
	}}
	text := err.Error()
	for _, want := range []string{
		"检测到 1 组重复的合同编号，迁移已中止，未写入任何数据。",
		"编号 GTSR-260001 出现 3 次，涉及租户：01JAAA,01JBBB",
		"请先清理或受控更正 con_contract.contract_number",
		"本迁移的存量回填可安全重跑",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("error text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "……其余") {
		t.Fatalf("单一分组不应出现剩余组提示：\n%s", text)
	}
}

// TestContractNumberConflictErrorTruncatesGroups 验证超过 20 组时只列前 20 组并说明剩余组数。
// 参数：t 为测试上下文。
// 返回值：无。
func TestContractNumberConflictErrorTruncatesGroups(t *testing.T) {
	groups := make([]DuplicateContractNumber, 0, 25)
	for index := 0; index < 25; index++ {
		groups = append(groups, conflictGroup(fmt.Sprintf("GTSR-%06d", index), nil, 2, "01JAAA", "01JBBB"))
	}
	text := (&ContractNumberConflictError{Groups: groups}).Error()
	if !strings.Contains(text, "检测到 25 组重复的合同编号") {
		t.Fatalf("error text missing total group count:\n%s", text)
	}
	if !strings.Contains(text, "……其余 5 组请用 cmd/contract-number-audit 查看完整清单") {
		t.Fatalf("error text missing remaining hint:\n%s", text)
	}
	if !strings.Contains(text, "GTSR-000019") || strings.Contains(text, "GTSR-000020") {
		t.Fatalf("error text must list exactly the first 20 groups:\n%s", text)
	}
}

// TestDuplicateContractNumberString 验证单组描述的三种写法。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDuplicateContractNumberString(t *testing.T) {
	cases := []struct {
		name  string
		group DuplicateContractNumber
		want  string
	}{
		{"原文即归一值", conflictGroup("GTSR-1", []string{"GTSR-1"}, 2, "t1", "t2"), "编号 GTSR-1 出现 2 次，涉及租户：t1,t2"},
		{"原文与归一值不同", conflictGroup("GTSR-1", []string{"gtsr-1"}, 2, "t1", "t2"), "编号 gtsr-1 出现 2 次，涉及租户：t1,t2（归一后 GTSR-1）"},
		{"多种写法", conflictGroup("GTSR-1", []string{"GTSR-1", "gtsr-1"}, 2, "t1", "t2"), "编号 GTSR-1 出现 2 次，涉及租户：t1,t2（归一后相同写法：GTSR-1、gtsr-1）"},
	}
	for _, testCase := range cases {
		if got := testCase.group.String(); got != testCase.want {
			t.Fatalf("%s: got %q want %q", testCase.name, got, testCase.want)
		}
	}
}

// TestDuplicateContractNumbersReleaseDecisionUsesExactGrouping 固定预检的放行口径：
// 只有「按 contract_number 原值分组」这一遍能决定放行，第二遍归一分组只补充报告写法。
//
// 这条语义对着两个真实风险：UPPER(TRIM()) 不考虑重音/全角等价（漏报），却把仅前导空格
// 不同算成一组（误报）。若让第二遍参与放行，就会既漏又误。本用例断言：
//  1. 第一遍无重复时，即使第二遍命中也不得拦住迁移（返回 nil）；
//  2. 第一遍有重复时，第二遍只补充写法与次数，不新增判定组；
//  3. 第二遍独有的分组（前导空格差异）不进入报告。
//
// 参数：t 为测试上下文。
// 返回值：无。
func TestDuplicateContractNumbersReleaseDecisionUsesExactGrouping(t *testing.T) {
	// 场景 1：前导空格差异只被第二遍看到，第一遍（与登记表主键语义一致）没有重复。
	informationalOnly := mergeDuplicateNumberReport(nil,
		[]DuplicateContractNumber{conflictGroup("GTSR-1", []string{"GTSR-1", " GTSR-1"}, 2, "t1", "t2")})
	if len(informationalOnly) != 0 {
		t.Fatalf("第二遍不得单独拦住迁移: %+v", informationalOnly)
	}

	// 场景 2：第一遍已判定重复，第二遍补充“归一后相同写法”与更大次数。
	released := []DuplicateContractNumber{conflictGroup("GTSR-1", []string{"gtsr-1"}, 2, "t1")}
	informational := []DuplicateContractNumber{
		conflictGroup("GTSR-1", []string{" GTSR-1", "gtsr-1"}, 3, "t2"),
		// 第二遍独有分组：仅前导空格不同，必须被丢弃。
		conflictGroup("GTSR-2", []string{" GTSR-2", "GTSR-2"}, 2, "t3"),
	}
	merged := mergeDuplicateNumberReport(released, informational)
	if len(merged) != 1 {
		t.Fatalf("第二遍不得新增判定组: %+v", merged)
	}
	group := merged[0]
	if group.Normalized != "GTSR-1" || group.Count != 3 {
		t.Fatalf("group=%+v want normalized GTSR-1 count 3", group)
	}
	if got := strings.Join(group.Numbers, ","); got != " GTSR-1,gtsr-1" {
		t.Fatalf("numbers=%q want 合并后的两种写法", got)
	}
	if got := strings.Join(group.Tenants, ","); got != "t1,t2" {
		t.Fatalf("tenants=%q", got)
	}

	// 场景 3：第二遍查询出错或为空时，第一遍判定结果原样保留。
	if only := mergeDuplicateNumberReport(released, nil); len(only) != 1 || only[0].Normalized != "GTSR-1" {
		t.Fatalf("缺少补充信息时判定组不得丢失: %+v", only)
	}
}

// TestMergeDuplicateNumberReportSortsByNormalized 验证报告分组按归一后编号排序，便于逐条复核。
// 参数：t 为测试上下文。
// 返回值：无。
func TestMergeDuplicateNumberReportSortsByNormalized(t *testing.T) {
	merged := mergeDuplicateNumberReport([]DuplicateContractNumber{
		conflictGroup("GTSR-9", []string{"GTSR-9"}, 2, "t1", "t2"),
		conflictGroup("GTSR-1", []string{"GTSR-1"}, 2, "t1", "t2"),
	}, nil)
	if len(merged) != 2 || merged[0].Normalized != "GTSR-1" || merged[1].Normalized != "GTSR-9" {
		t.Fatalf("merged=%+v", merged)
	}
}

// TestSplitConcatenated 验证聚合结果拆分时去掉空片段。
// 参数：t 为测试上下文。
// 返回值：无。
func TestSplitConcatenated(t *testing.T) {
	if got := splitConcatenated("", ","); got != nil {
		t.Fatalf("splitConcatenated(\"\") = %#v", got)
	}
	if got := strings.Join(splitConcatenated("t1,,t2", ","), "|"); got != "t1|t2" {
		t.Fatalf("splitConcatenated = %q", got)
	}
}

// TestAuditReportCleanAndTotal 验证巡检报告的干净判定与问题合计。
// 参数：t 为测试上下文。
// 返回值：无。
func TestAuditReportCleanAndTotal(t *testing.T) {
	clean := AuditReport{}
	if !clean.Clean() || clean.Total() != 0 {
		t.Fatalf("zero report clean=%v total=%d", clean.Clean(), clean.Total())
	}
	dirty := AuditReport{
		Duplicates:        []DuplicateContractNumber{conflictGroup("GTSR-1", nil, 2, "t1", "t2")},
		UnregisteredTotal: 2,
		MismatchedTotal:   4,
	}
	if dirty.Clean() || dirty.Total() != 7 {
		t.Fatalf("dirty report clean=%v total=%d, want false/7", dirty.Clean(), dirty.Total())
	}
}
