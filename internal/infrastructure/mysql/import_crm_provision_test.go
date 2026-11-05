package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/testutil/scriptdb"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// importProvisionSQLRepository 构造执行真实 Gorm 路径的脚本 SQL 仓储，不连接 MySQL。
// 参数：t 为测试上下文，steps 为预期的连接/事务/SQL 边界。
// 返回值：仓储；实际 SQL 与脚本不符时测试失败。
func importProvisionSQLRepository(t *testing.T, steps ...scriptdb.Step) *Repository {
	t.Helper()
	db := scriptdb.Open(t, steps...)
	gormDB, err := gorm.Open(gormmysql.New(gormmysql.Config{Conn: db, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	return &Repository{db: gormDB}
}

// provisionSQLKey 返回符合主键列约束的测试幂等键。
// 参数：无。
// 返回值：26 位租户 + 合法来源系统/外部记录 ID。
func provisionSQLKey() contract.ImportCRMProvisionKey {
	return contract.ImportCRMProvisionKey{
		TenantID:         "01J00000000000000000000000",
		SourceSystem:     "crm",
		ExternalRecordID: "history-0001",
	}
}

// provisionSQLRowNames 是 con_import_crm_provision 的列序（迁移 000043）。
// 参数：无。
// 返回值：SELECT * 结果需要的列名。
func provisionSQLRowNames() []string {
	return []string{"tenant_id", "source_system", "external_record_id", "batch_id", "customer_id", "customer_created",
		"opportunity_id", "opportunity_created", "state", "attempt", "last_error_code", "last_error_message", "created_at", "updated_at"}
}

// provisionSQLArgValues 提取脚本驱动收集到的绑定参数值。
// 参数：values 为驱动参数。
// 返回值：只含值的切片，便于断言关键绑定。
func provisionSQLArgValues(values []driver.NamedValue) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value.Value)
	}
	return result
}

// provisionSQLContainsArg 判断绑定参数里是否出现期望值。
// 参数：values 为参数值切片，expected 为期望值。
// 返回值：出现返回 true。
func provisionSQLContainsArg(values []any, expected any) bool {
	for _, value := range values {
		if reflect.DeepEqual(value, expected) {
			return true
		}
	}
	return false
}

// TestGetImportCRMProvisionMapsRowAndMissingKey 验证按幂等键读取与不存在分类。
// 参数：t 为测试上下文。
// 返回值：无；列映射或错误分类不符时失败。
func TestGetImportCRMProvisionMapsRowAndMissingKey(t *testing.T) {
	key := provisionSQLKey()
	now := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	repo := importProvisionSQLRepository(t, scriptdb.Step{
		Kind: "query", SQL: "con_import_crm_provision", Names: provisionSQLRowNames(),
		Rows: [][]driver.Value{{
			key.TenantID, key.SourceSystem, key.ExternalRecordID, "batch-0001", uint64(7), int64(1),
			uint64(9), int64(0), contract.ImportCRMProvisionCustomerReady, int64(1), nil, nil, now, now,
		}},
	})
	value, err := repo.GetImportCRMProvision(context.Background(), key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if value.CustomerID != 7 || !value.CustomerCreated || value.OpportunityID != 9 || value.OpportunityCreated ||
		value.State != contract.ImportCRMProvisionCustomerReady || value.Attempt != 1 || value.BatchID != "batch-0001" {
		t.Fatalf("mapped value=%+v", value)
	}
	missing := importProvisionSQLRepository(t, scriptdb.Step{Kind: "query", SQL: "con_import_crm_provision", Names: provisionSQLRowNames()})
	if _, err := missing.GetImportCRMProvision(context.Background(), key); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatalf("missing record error=%v", err)
	}
	// 非法幂等键在任何 SQL 之前失败关闭。
	noSQL := importProvisionSQLRepository(t)
	if _, err := noSQL.GetImportCRMProvision(context.Background(), contract.ImportCRMProvisionKey{TenantID: "short"}); !errors.Is(err, contract.ErrImportCRMProvisionKey) {
		t.Fatalf("invalid key error=%v", err)
	}
}

// TestUpsertStartImportCRMProvisionIsIdempotentOnKey 验证起点写入使用唯一键幂等语句，
// 且返回台账当前状态（不回退已完成进度）。
// 参数：t 为测试上下文。
// 返回值：无；SQL 形态、绑定值或返回映射不符时失败。
func TestUpsertStartImportCRMProvisionIsIdempotentOnKey(t *testing.T) {
	key := provisionSQLKey()
	now := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	var args []driver.NamedValue
	var statement string
	repo := importProvisionSQLRepository(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "INSERT INTO con_import_crm_provision", Args: &args, SQLOut: &statement},
		scriptdb.Step{Kind: "query", SQL: "con_import_crm_provision", Names: provisionSQLRowNames(),
			Rows: [][]driver.Value{{key.TenantID, key.SourceSystem, key.ExternalRecordID, "batch-1", uint64(0), int64(0),
				uint64(0), int64(0), contract.ImportCRMProvisionCompleted, int64(3), nil, nil, now, now}}},
		scriptdb.Step{Kind: "commit"},
	)
	value, err := repo.UpsertStartImportCRMProvision(context.Background(), key, "batch-1")
	if err != nil {
		t.Fatalf("upsert start: %v", err)
	}
	if value.State != contract.ImportCRMProvisionCompleted || value.Attempt != 3 || value.BatchID != "batch-1" {
		t.Fatalf("completed state regressed: %+v", value)
	}
	// 重跑只允许 attempt+1 与刷新批次，绝不重置 state/已取得的 CRM 标识。
	if !strings.Contains(statement, "ON DUPLICATE KEY UPDATE") || !strings.Contains(statement, "attempt=attempt+1") ||
		strings.Contains(statement, "state=VALUES") || strings.Contains(statement, "customer_id=VALUES") {
		t.Fatalf("upsert statement=%s", statement)
	}
	values := provisionSQLArgValues(args)
	for _, expected := range []any{key.TenantID, key.SourceSystem, key.ExternalRecordID, "batch-1", contract.ImportCRMProvisionPending} {
		if !provisionSQLContainsArg(values, expected) {
			t.Fatalf("upsert args=%v missing %v", values, expected)
		}
	}
}

// TestAdvanceImportCRMProvisionUsesStateAttemptCAS 验证推进以 state+attempt 为条件，
// 并同语句写入本次取得的 CRM 标识。
// 参数：t 为测试上下文。
// 返回值：无；CAS 条件、绑定值或读回映射不符时失败。
func TestAdvanceImportCRMProvisionUsesStateAttemptCAS(t *testing.T) {
	key := provisionSQLKey()
	now := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	var args []driver.NamedValue
	var statement string
	repo := importProvisionSQLRepository(t,
		// Gorm 默认把 Updates 包在事务里：begin → UPDATE → commit，随后单独读回台账。
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE `con_import_crm_provision`", Args: &args, SQLOut: &statement},
		scriptdb.Step{Kind: "commit"},
		scriptdb.Step{Kind: "query", SQL: "con_import_crm_provision", Names: provisionSQLRowNames(),
			Rows: [][]driver.Value{{key.TenantID, key.SourceSystem, key.ExternalRecordID, nil, uint64(42), int64(1),
				nil, int64(0), contract.ImportCRMProvisionCustomerReady, int64(2), nil, nil, now, now}}},
	)
	advance := contract.ImportCRMProvisionAdvance{
		From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionCustomerReady,
		ExpectedAttempt: 1, CustomerID: 42, CustomerCreated: true,
	}
	value, err := repo.AdvanceImportCRMProvision(context.Background(), key, advance)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if value.State != contract.ImportCRMProvisionCustomerReady || value.CustomerID != 42 {
		t.Fatalf("advance result=%+v", value)
	}
	if !strings.Contains(statement, "state=? AND attempt=?") {
		t.Fatalf("advance CAS missing: %s", statement)
	}
	values := provisionSQLArgValues(args)
	for _, expected := range []any{
		key.TenantID, key.SourceSystem, key.ExternalRecordID,
		contract.ImportCRMProvisionPending, int64(advance.ExpectedAttempt), contract.ImportCRMProvisionCustomerReady,
		int64(advance.ExpectedAttempt + 1), int64(42), true,
	} {
		if !provisionSQLContainsArg(values, expected) {
			t.Fatalf("advance args=%v missing %v", values, expected)
		}
	}
}

// TestAdvanceImportCRMProvisionRejectsIllegalTransitionBeforeSQL 验证非法推进不触达数据库。
// 参数：t 为测试上下文。
// 返回值：无；跳步或缺少 CRM 标识仍执行 SQL 时失败。
func TestAdvanceImportCRMProvisionRejectsIllegalTransitionBeforeSQL(t *testing.T) {
	repo := importProvisionSQLRepository(t) // 无脚本：任何 SQL 都会被脚本驱动判为越界。
	key := provisionSQLKey()
	for _, advance := range []contract.ImportCRMProvisionAdvance{
		{From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionContractReady},
		{From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionCustomerReady},
		{From: contract.ImportCRMProvisionCompleted, To: contract.ImportCRMProvisionBlocked},
		{From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionCustomerReady, ExpectedAttempt: -1, CustomerID: 1},
	} {
		if _, err := repo.AdvanceImportCRMProvision(context.Background(), key, advance); !errors.Is(err, contract.ErrImportCRMProvisionState) {
			t.Fatalf("advance %+v error=%v", advance, err)
		}
	}
	if _, err := repo.AdvanceImportCRMProvision(context.Background(), contract.ImportCRMProvisionKey{TenantID: "short"}, contract.ImportCRMProvisionAdvance{
		From: contract.ImportCRMProvisionPending, To: contract.ImportCRMProvisionCustomerReady, CustomerID: 1,
	}); !errors.Is(err, contract.ErrImportCRMProvisionKey) {
		t.Fatalf("invalid key error=%v", err)
	}
}

// TestRecordImportCRMProvisionErrorBlocksAndTruncates 验证失败落账写 BLOCKED、
// 保留 attempt 并按列宽截断消息。
// 参数：t 为测试上下文。
// 返回值：无；状态、截断或绑定值不符时失败。
func TestRecordImportCRMProvisionErrorBlocksAndTruncates(t *testing.T) {
	key := provisionSQLKey()
	now := time.Date(2026, 11, 5, 1, 2, 3, 0, time.UTC)
	var args []driver.NamedValue
	var statement string
	repo := importProvisionSQLRepository(t,
		scriptdb.Step{Kind: "begin"},
		scriptdb.Step{Kind: "exec", SQL: "UPDATE `con_import_crm_provision`", Args: &args, SQLOut: &statement},
		scriptdb.Step{Kind: "commit"},
		scriptdb.Step{Kind: "query", SQL: "con_import_crm_provision", Names: provisionSQLRowNames(),
			Rows: [][]driver.Value{{key.TenantID, key.SourceSystem, key.ExternalRecordID, nil, uint64(42), int64(1),
				nil, int64(0), contract.ImportCRMProvisionBlocked, int64(1), "IMPORT_PROVISION_CUSTOMER_AMBIGUOUS",
				strings.Repeat("合", 170), now, now}}},
	)
	long := strings.Repeat("合", 300)
	value, err := repo.RecordImportCRMProvisionError(context.Background(), key, contract.ImportCRMProvisionCustomerReady, 1, "IMPORT_PROVISION_CUSTOMER_AMBIGUOUS", long)
	if err != nil {
		t.Fatalf("record error: %v", err)
	}
	if value.State != contract.ImportCRMProvisionBlocked || value.LastErrorCode != "IMPORT_PROVISION_CUSTOMER_AMBIGUOUS" {
		t.Fatalf("blocked value=%+v", value)
	}
	if !strings.Contains(statement, "state=? AND attempt=?") {
		t.Fatalf("record error CAS missing: %s", statement)
	}
	values := provisionSQLArgValues(args)
	for _, expected := range []any{key.TenantID, key.SourceSystem, key.ExternalRecordID, contract.ImportCRMProvisionCustomerReady, int64(1), contract.ImportCRMProvisionBlocked} {
		if !provisionSQLContainsArg(values, expected) {
			t.Fatalf("record error args=%v missing %v", values, expected)
		}
	}
	messageBound := false
	for _, value := range values {
		if text, ok := value.(string); ok && len(text) == contract.ImportCRMProvisionMaxErrorMessageBytes-2 && strings.HasPrefix(text, "合") {
			messageBound = true
		}
	}
	if !messageBound {
		t.Fatalf("message was not truncated to column width: %v", values)
	}
	// 空错误码必须失败关闭且不写库。
	noSQL := importProvisionSQLRepository(t)
	if _, err := noSQL.RecordImportCRMProvisionError(context.Background(), key, contract.ImportCRMProvisionPending, 0, "", "x"); !errors.Is(err, contract.ErrImportCRMProvisionError) {
		t.Fatalf("empty code error=%v", err)
	}
}
