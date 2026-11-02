package migration

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/migrations"
	"github.com/oklog/ulid/v2"
)

// 本文件是合同编号预检与巡检的 E2E 用例：只在显式隔离的 contract_test_* 回环库上运行，
// 未配置 CONTRACT_TEST_DSN 时整体跳过，绝不连接业务库。

// preflightE2EDatabase 打开本文件专用的隔离测试库，并先迁移到最新版本。
//
// 参数：t 为测试上下文；要求 CONTRACT_TEST_DSN 指向 contract_test_* 回环 TCP 库。
// 返回值：连接池（测试结束自动关闭）与该库的 DSN；未配置时跳过用例。
func preflightE2EDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("CONTRACT_TEST_DSN")
	if dsn == "" {
		t.Skip("CONTRACT_TEST_DSN is not configured; no database connection attempted")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "contract_test_") || cfg.Net != "tcp" ||
		!(strings.HasPrefix(cfg.Addr, "127.0.0.1:") || strings.HasPrefix(cfg.Addr, "localhost:")) {
		t.Fatal("CONTRACT_TEST_DSN must target an explicitly named contract_test_* database on loopback TCP")
	}
	cfg.ParseTime = true
	normalized := cfg.FormatDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := Run(ctx, normalized, migrations.Files); err != nil {
		t.Fatalf("dedicated test database migrations failed: %v", err)
	}
	database, err := sql.Open("mysql", normalized)
	if err != nil {
		t.Fatalf("dedicated test database connection failed: %v", err)
	}
	database.SetMaxOpenConns(4)
	database.SetMaxIdleConns(4)
	t.Cleanup(func() { database.Close() })
	return database, normalized
}

// preflightE2EContract 直接插入一条合同行，绕过取号与仓储层，用来制造预检与巡检要看的数据。
//
// 参数：t 为测试上下文，database 为隔离库连接，tenantID/contractID 为租户与合同主键，
// number 为合同编号。
// 返回值：无。
func preflightE2EContract(t *testing.T, database *sql.DB, tenantID, contractID, number string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	_, err := database.ExecContext(context.Background(), `INSERT INTO con_contract
		(id, tenant_id, contract_number, title, contract_type, service_type, owner_user_id,
		 amount_minor, currency, content, status, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, '编号预检用例', 'sales', 'service', ?, 1, 'CNY', '{}', 'draft', ?, 'preflight-e2e', ?, 'preflight-e2e')`,
		contractID, tenantID, number, ulid.Make().String(), now, now)
	if err != nil {
		t.Fatalf("insert contract row for preflight case failed: %v", err)
	}
}

// preflightE2ERegistry 直接插入一条登记表记录，用来制造登记错配。
//
// 参数：t 为测试上下文，database 为隔离库连接，number 为编号，tenantID/contractID 为
// 登记表写下的租户与合同（可与合同行故意不同）。
// 返回值：无。
func preflightE2ERegistry(t *testing.T, database *sql.DB, number, tenantID, contractID string) {
	t.Helper()
	_, err := database.ExecContext(context.Background(), `INSERT INTO `+registryTable+`
		(contract_number, tenant_id, contract_id, seq_year, rule_key) VALUES (?, ?, ?, 0, '')`,
		number, tenantID, contractID)
	if err != nil {
		t.Fatalf("insert registry row for audit case failed: %v", err)
	}
}

// preflightE2ECleanupTenant 只按租户精确清理本文件造的数据，不做全表删除。
//
// 参数：t 为测试上下文，database 为隔离库连接，tenantIDs 为待清理租户。
// 返回值：无。
func preflightE2ECleanupTenant(t *testing.T, database *sql.DB, tenantIDs ...string) {
	t.Helper()
	for _, tenantID := range tenantIDs {
		if tenantID == "" {
			continue
		}
		for _, table := range []string{registryTable, contractTable} {
			// 用例 C 会故意把 000037 回退成未应用并 DROP 登记表。清理必须跳过缺失的表，
			// 否则「预检确实拦住了迁移」这个成功用例会被 cleanup 的错误判成失败。
			exists, err := tableExists(context.Background(), database, table)
			if err != nil {
				t.Errorf("检查表 %s 是否存在失败：%v", table, err)
				continue
			}
			if !exists {
				continue
			}
			if _, err := database.ExecContext(context.Background(),
				`DELETE FROM `+table+` WHERE tenant_id = ?`, tenantID); err != nil {
				t.Errorf("cleanup %s for test tenant failed: %v", table, err)
			}
		}
	}
}

// preflightE2ERevertRegistryMigration 把 000037 回退成“未应用”，让 Run 必须经过预检。
//
// 参数：t 为测试上下文，database 为隔离库连接。
// 返回值：无。
func preflightE2ERevertRegistryMigration(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.ExecContext(context.Background(),
		`DELETE FROM `+metadataTable+` WHERE version = ?`, 37); err != nil {
		t.Fatalf("revert migration 37 record failed: %v", err)
	}
	if _, err := database.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+registryTable+``); err != nil {
		t.Fatalf("drop contract number registry failed: %v", err)
	}
}

// containsDuplicate 判断报告中是否包含指定编号的重复组。
func containsDuplicate(groups []DuplicateContractNumber, number string) bool {
	for _, group := range groups {
		if group.Normalized == number {
			return true
		}
		for _, variant := range group.Numbers {
			if variant == number {
				return true
			}
		}
	}
	return false
}

// TestPreflightContractNumbersDetectsCrossTenantDuplicate 覆盖用例 A、B：
// 跨租户重号必须被预检拦下并指名编号与租户，清理后预检恢复通过。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestPreflightContractNumbersDetectsCrossTenantDuplicate(t *testing.T) {
	database, _ := preflightE2EDatabase(t)
	ctx := context.Background()
	tenantA, tenantB := ulid.Make().String(), ulid.Make().String()
	t.Cleanup(func() { preflightE2ECleanupTenant(t, database, tenantA, tenantB) })
	number := "GTSR-PREFLIGHT-" + strings.ToUpper(ulid.Make().String()[:8])
	// 租户内唯一键是 uk_con_contract_number(tenant_id, contract_number)，
	// 因此同号必须落在两个租户上才能制造跨租户重号。
	preflightE2EContract(t, database, tenantA, ulid.Make().String(), number)
	preflightE2EContract(t, database, tenantB, ulid.Make().String(), number)

	err := PreflightContractNumbers(ctx, database)
	if err == nil {
		t.Fatalf("跨租户重号 %s 必须被预检拦下", number)
	}
	message := err.Error()
	for _, want := range []string{
		"检测到 ", "组重复的合同编号，迁移已中止，未写入任何数据",
		"编号 " + number + " 出现 2 次，涉及租户：",
		tenantA, tenantB,
		"请先清理或受控更正 con_contract.contract_number",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("预检错误缺少 %q：\n%s", want, message)
		}
	}
	// 报告必须完全中文化：不能把裸 MySQL 报错（如 1062 主键冲突）抛给运维。
	for _, forbidden := range []string{"Error 1062", "Duplicate entry", "Error 1054", "sql: "} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("预检错误不应包含 %q：\n%s", forbidden, message)
		}
	}

	// 用例 B：清理重号后预检通过，证明拦截来自真实数据而不是常量错误。
	preflightE2ECleanupTenant(t, database, tenantA, tenantB)
	if err := PreflightContractNumbers(ctx, database); err != nil {
		t.Fatalf("清理重号后预检必须通过，实际错误：%v", err)
	}
}

// TestRunPreflightAbortsRegistryMigration 覆盖用例 C：
// Run 的自动预检在存在重号时中止，且不执行 000037 的任何语句。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestRunPreflightAbortsRegistryMigration(t *testing.T) {
	database, dsn := preflightE2EDatabase(t)
	ctx := context.Background()
	tenantA, tenantB := ulid.Make().String(), ulid.Make().String()
	number := "GTSR-PREFLIGHT-RUN-" + strings.ToUpper(ulid.Make().String()[:8])
	t.Cleanup(func() {
		preflightE2ECleanupTenant(t, database, tenantA, tenantB)
		// 本用例把 000037 回退成未应用，必须恢复，不能把隔离库留在半迁移状态。
		if err := Run(context.Background(), dsn, migrations.Files); err != nil {
			t.Errorf("restore migration state after preflight case failed: %v", err)
		}
	})
	preflightE2EContract(t, database, tenantA, ulid.Make().String(), number)
	preflightE2EContract(t, database, tenantB, ulid.Make().String(), number)
	preflightE2ERevertRegistryMigration(t, database)

	err := Run(ctx, dsn, migrations.Files)
	if err == nil {
		t.Fatal("存在跨租户重号时 Run 必须中止")
	}
	if !strings.Contains(err.Error(), "重复的合同编号") {
		t.Fatalf("Run 错误应指出来自编号预检，实际：%v", err)
	}
	// 迁移语句没有执行：登记表仍不存在。
	exists, checkErr := tableExists(ctx, database, registryTable)
	if checkErr != nil {
		t.Fatalf("check registry table failed: %v", checkErr)
	}
	if exists {
		t.Fatal("预检失败后 000037 的建表语句不得执行")
	}
	// 迁移记录也没有写回：con_schema_migration 里仍无 37。
	var applied int
	if scanErr := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+metadataTable+` WHERE version = ?`, 37).Scan(&applied); scanErr != nil {
		t.Fatalf("read applied migration failed: %v", scanErr)
	}
	if applied != 0 {
		t.Fatalf("预检失败后不得写入 37 号迁移记录，实际记录数 %d", applied)
	}
}

// TestAuditContractNumberRegistryReportsProblems 覆盖用例 D：
// 巡检能分别报出「未登记」与「登记错配」，且不把错配编号误判为未登记。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestAuditContractNumberRegistryReportsProblems(t *testing.T) {
	database, _ := preflightE2EDatabase(t)
	ctx := context.Background()
	tenantA, tenantB := ulid.Make().String(), ulid.Make().String()
	t.Cleanup(func() { preflightE2ECleanupTenant(t, database, tenantA, tenantB) })
	suffix := strings.ToUpper(ulid.Make().String()[:8])
	// 编号前缀 AUDIT- 在字典序上靠前，保证明细被展示而不是被 20 条上限截断。
	unregisteredNumber := "AUDIT-UNREG-" + suffix
	mismatchNumber := "AUDIT-MISMATCH-" + suffix
	unregisteredID, mismatchID := ulid.Make().String(), ulid.Make().String()
	preflightE2EContract(t, database, tenantA, unregisteredID, unregisteredNumber)
	preflightE2EContract(t, database, tenantB, mismatchID, mismatchNumber)
	// 登记表故意记成别的租户与别的合同：同时覆盖租户错配与合同 ID 错配。
	preflightE2ERegistry(t, database, mismatchNumber, tenantA, ulid.Make().String())

	report, err := AuditContractNumberRegistry(ctx, database)
	if err != nil {
		t.Fatalf("AuditContractNumberRegistry() error = %v", err)
	}
	if report.Clean() {
		t.Fatal("存在未登记与登记错配时报告不得判定为干净")
	}
	if !containsUnregistered(report.Unregistered, unregisteredNumber, tenantA, unregisteredID) {
		t.Fatalf("巡检未报出未登记编号 %s：%+v", unregisteredNumber, report.Unregistered)
	}
	if containsUnregistered(report.Unregistered, mismatchNumber, tenantB, mismatchID) {
		t.Fatalf("已登记（即使错配）的编号不得算作未登记：%+v", report.Unregistered)
	}
	if !containsMismatch(report.Mismatched, mismatchNumber, tenantB, mismatchID, tenantA) {
		t.Fatalf("巡检未报出登记错配 %s：%+v", mismatchNumber, report.Mismatched)
	}
	if containsDuplicate(report.Duplicates, unregisteredNumber) || containsDuplicate(report.Duplicates, mismatchNumber) {
		t.Fatalf("本用例编号唯一，不应被判为重复：%+v", report.Duplicates)
	}
	if report.UnregisteredTotal < 1 || report.MismatchedTotal < 1 {
		t.Fatalf("未登记/错配总数应为正：%+v", report)
	}
}

// containsUnregistered 判断未登记明细里是否有指定编号、租户与合同。
func containsUnregistered(details []UnregisteredContractNumber, number, tenantID, contractID string) bool {
	for _, detail := range details {
		if detail.ContractNumber == number && detail.TenantID == tenantID && detail.ContractID == contractID {
			return true
		}
	}
	return false
}

// containsMismatch 判断错配明细里是否有指定的合同行与登记表两侧取值。
func containsMismatch(details []RegistryMismatch, number, contractTenantID, contractID, registryTenantID string) bool {
	for _, detail := range details {
		if detail.ContractNumber == number && detail.ContractTenantID == contractTenantID &&
			detail.ContractID == contractID && detail.RegistryTenantID == registryTenantID {
			return true
		}
	}
	return false
}

// TestRunResumesRegistryBackfillAfterInterruption 覆盖 000037 的可续跑回填：
// 进程若在「回填已提交、con_schema_migration 记账前」被中断，重跑必须成功，
// 而不是对每个已登记编号报 1062 让运维误以为存在重号。
//
// fail-closed 语义不受影响：真重号在预检阶段就被拦下（见
// TestRunPreflightAbortsRegistryMigration），本用例只验证“续跑不误报”。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestRunResumesRegistryBackfillAfterInterruption(t *testing.T) {
	database, dsn := preflightE2EDatabase(t)
	ctx := context.Background()
	tenant := ulid.Make().String()
	t.Cleanup(func() {
		preflightE2ECleanupTenant(t, database, tenant)
		// 即使用例中途失败，也要把隔离库带回“已迁移到最新”的状态。
		if err := Run(context.Background(), dsn, migrations.Files); err != nil {
			t.Errorf("restore migration state after resume case failed: %v", err)
		}
	})
	number := "GTSR-RESUME-" + strings.ToUpper(ulid.Make().String()[:8])
	contractID := ulid.Make().String()
	preflightE2EContract(t, database, tenant, contractID, number)
	// 模拟“回填已提交”：登记行已存在。
	preflightE2ERegistry(t, database, number, tenant, contractID)
	// 模拟“metadata 未写”：只删迁移记录，保留登记表（这正是中断窗口的状态）。
	if _, err := database.ExecContext(ctx,
		`DELETE FROM `+metadataTable+` WHERE version = ?`, 37); err != nil {
		t.Fatalf("revert migration 37 record failed: %v", err)
	}

	// 重跑：回填语句必须跳过已登记编号，不能报重复键。
	if err := Run(ctx, dsn, migrations.Files); err != nil {
		t.Fatalf("回填已提交但未记账时重跑迁移必须成功，实际：%v", err)
	}
	// 登记表里该号仍然恰好一行，且没有被“覆盖归属”。
	var rows int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+registryTable+` WHERE contract_number = ?`, number).Scan(&rows); err != nil {
		t.Fatalf("read registry rows failed: %v", err)
	}
	if rows != 1 {
		t.Fatalf("续跑后登记行数=%d want 1（必须跳过已登记编号）", rows)
	}
	var owner string
	if err := database.QueryRowContext(ctx,
		`SELECT tenant_id FROM `+registryTable+` WHERE contract_number = ?`, number).Scan(&owner); err != nil {
		t.Fatalf("read registry owner failed: %v", err)
	}
	if owner != tenant {
		t.Fatalf("续跑改动了登记归属：got %s want %s", owner, tenant)
	}
	// 迁移记录已补写。
	var applied int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+metadataTable+` WHERE version = ?`, 37).Scan(&applied); err != nil {
		t.Fatalf("read applied migration failed: %v", err)
	}
	if applied != 1 {
		t.Fatalf("续跑后必须补写 37 号迁移记录，实际 %d", applied)
	}
}
