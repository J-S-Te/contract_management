package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/migration"
)

// testDuplicateGroup 构造一组重号，避免用例重复拼装。
func testDuplicateGroup(number, tenantA, tenantB string) migration.DuplicateContractNumber {
	return migration.DuplicateContractNumber{
		Normalized: number, Numbers: []string{number}, Count: 2, Tenants: []string{tenantA, tenantB},
	}
}

// TestRunRejectsMissingDSN 验证缺少连接串时不连接数据库直接失败。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRunRejectsMissingDSN(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(&stdout, &stderr, ""); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "MYSQL_DSN 未配置") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestRunRejectsDSNWithoutDatabase 验证连接串缺少库名时直接失败。
// 参数：t 为测试上下文。
// 返回值：无。
func TestRunRejectsDSNWithoutDatabase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(&stdout, &stderr, "audit:audit@tcp(127.0.0.1:3306)/"); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "未指定数据库名") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestWriteReportClean 验证无问题时输出中文一致结论。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWriteReportClean(t *testing.T) {
	var output bytes.Buffer
	writeReport(&output, migration.AuditReport{})
	text := output.String()
	if !strings.Contains(text, "合同编号与登记表一致") || strings.Contains(text, "结论：共发现") {
		t.Fatalf("output = %q", text)
	}
	for _, section := range []string{"[重复编号] 共 0 组", "[未登记] 共 0 条", "[登记错配] 共 0 条"} {
		if !strings.Contains(text, section) {
			t.Fatalf("output missing %q: %q", section, text)
		}
	}
}

// TestWriteReportProblems 验证三类问题都按中文口径列出并给出总数结论。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWriteReportProblems(t *testing.T) {
	report := migration.AuditReport{
		Duplicates:        []migration.DuplicateContractNumber{testDuplicateGroup("GTSR-1", "tenant-a", "tenant-b")},
		Unregistered:      []migration.UnregisteredContractNumber{{ContractNumber: "GTSR-2", TenantID: "tenant-a", ContractID: "c-2"}},
		UnregisteredTotal: 3,
		Mismatched: []migration.RegistryMismatch{{
			ContractNumber: "GTSR-3", ContractTenantID: "tenant-a", ContractID: "c-3",
			RegistryTenantID: "tenant-b", RegistryContractID: "c-9",
		}},
		MismatchedTotal: 1,
	}
	if report.Clean() || report.Total() != 5 {
		t.Fatalf("clean=%v total=%d, want false/5", report.Clean(), report.Total())
	}
	var output bytes.Buffer
	if code := emitReport(&output, report); code != 1 {
		t.Fatalf("发现问题时退出码 = %d, want 1", code)
	}
	text := output.String()
	for _, want := range []string{
		"编号 GTSR-1 出现 2 次，涉及租户：tenant-a,tenant-b",
		"编号 GTSR-2 未登记，租户 tenant-a，合同 c-2",
		"编号 GTSR-3 登记错配：合同行租户 tenant-a / 合同 c-3，登记表租户 tenant-b / 合同 c-9",
		"共发现 5 处问题（重复编号 1 组、未登记 3 条、登记错配 1 条）",
		"其余 2 条未显示",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

// TestEmitReportCleanExitCode 验证干净报告的退出码为 0。
// 参数：t 为测试上下文。
// 返回值：无。
func TestEmitReportCleanExitCode(t *testing.T) {
	var output bytes.Buffer
	if code := emitReport(&output, migration.AuditReport{}); code != 0 {
		t.Fatalf("干净报告退出码 = %d, want 0", code)
	}
	if !strings.Contains(output.String(), "合同编号与登记表一致") {
		t.Fatalf("output = %q", output.String())
	}
}

// TestWriteReportTruncatesDuplicates 验证重复组超过 20 组时说明剩余数量。
// 参数：t 为测试上下文。
// 返回值：无。
func TestWriteReportTruncatesDuplicates(t *testing.T) {
	groups := make([]migration.DuplicateContractNumber, 0, 25)
	for index := 0; index < 25; index++ {
		number := fmt.Sprintf("GTSR-%02d", index)
		groups = append(groups, testDuplicateGroup(number, "tenant-a", "tenant-b"))
	}
	var output bytes.Buffer
	writeReport(&output, migration.AuditReport{Duplicates: groups})
	text := output.String()
	if !strings.Contains(text, "[重复编号] 共 25 组") || !strings.Contains(text, "其余 5 条未显示") {
		t.Fatalf("output = %q", text)
	}
	if strings.Contains(text, "GTSR-24") {
		t.Fatalf("output must stop at 20 lines: %q", text)
	}
}
