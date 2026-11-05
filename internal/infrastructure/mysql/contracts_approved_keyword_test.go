package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// approvedContractSQLRecorder 捕获 DryRun 生成的原始 SQL 与绑定参数。
// 用 ParamsFilter 而不是 Trace 的 Explain 结果，是为了断言真正绑定的
// 关键字模式（Explain 会把反斜杠再转义一层，难以稳定比对）。
type approvedContractSQLRecorder struct {
	sql    string
	params []any
	// sqls/paramSets 保留本次查询生成的全部语句（分页台账是“先 COUNT 再取页”两条），
	// 只留最后一条会让 COUNT 语句的数据范围失去断言（跨租户总数泄漏无法被发现）。
	sqls     []string
	paramSets [][]any
}

func (r *approvedContractSQLRecorder) LogMode(logger.LogLevel) logger.Interface      { return r }
func (r *approvedContractSQLRecorder) Info(context.Context, string, ...interface{})  {}
func (r *approvedContractSQLRecorder) Warn(context.Context, string, ...interface{})  {}
func (r *approvedContractSQLRecorder) Error(context.Context, string, ...interface{}) {}
func (r *approvedContractSQLRecorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	fc()
}
func (r *approvedContractSQLRecorder) ParamsFilter(_ context.Context, sql string, params ...interface{}) (string, []interface{}) {
	r.sql = sql
	r.params = append([]any(nil), params...)
	r.sqls = append(r.sqls, sql)
	r.paramSets = append(r.paramSets, append([]any(nil), params...))
	return sql, params
}

// 本用例不需要 CONTRACT_TEST_DSN：DryRun 只生成 SQL、不建立连接。
func newApprovedContractDryRun(t *testing.T) (*gorm.DB, *approvedContractSQLRecorder) {
	t.Helper()
	recorder := &approvedContractSQLRecorder{}
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		DSN:                       "test:test@tcp(localhost:3306)/test?parseTime=true",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true, Logger: recorder})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return db, recorder
}

// boundKeywordPatterns 取出 SQL 实际绑定的关键字模式参数。
func boundKeywordPatterns(params []any) []string {
	patterns := make([]string, 0, 3)
	for _, value := range params {
		text, ok := value.(string)
		if ok && strings.Contains(text, "%") {
			patterns = append(patterns, text)
		}
	}
	return patterns
}

// TestListApprovedContractReferencesKeywordEscapesLikeLiterals 固定关键字过滤口径：
// 合同编号/标题/客户名三列同时匹配、% _ \ 必须按字面量转义、租户与来源谓词不能丢失。
func TestListApprovedContractReferencesKeywordEscapesLikeLiterals(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	if _, err := repository.ListApprovedContractReferences(context.Background(), "tenant-1", " 50%_x\\y ", "C-1", 500); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{
		"tenant_id = ?", "status IN (",
		"contract_number LIKE ?", "title LIKE ?", "customer_name LIKE ?",
		"COALESCE(NULLIF(origin_kind,''),'NORMAL')='NORMAL'", "id > ?",
		"ORDER BY id ASC", "LIMIT ?",
	} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("引用查询缺少 %q: %s", fragment, recorder.sql)
		}
	}
	patterns := boundKeywordPatterns(recorder.params)
	want := `%50\%\_x\\y%`
	if len(patterns) != 3 {
		t.Fatalf("关键字应绑定 3 个 LIKE 模式，实际 %d 个: %v", len(patterns), recorder.params)
	}
	for _, pattern := range patterns {
		if pattern != want {
			t.Fatalf("LIKE 模式=%q，want %q", pattern, want)
		}
	}
	// 反向验证：未转义的 % 会被当成通配符，整表命中。这里必须不出现裸模式。
	for _, pattern := range patterns {
		if pattern == "%50%_x\\y%" {
			t.Fatalf("关键字模式未转义: %q", pattern)
		}
	}
}

// TestListApprovedContractReferencesWithoutKeywordKeepsOldShape 固定空关键字行为：
// 不追加 LIKE 谓词，游标与 limit 钳制不变，旧调用方拿到的仍是原来的有界页。
func TestListApprovedContractReferencesWithoutKeywordKeepsOldShape(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	if _, err := repository.ListApprovedContractReferences(context.Background(), "tenant-1", "   ", "", 1000); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	if strings.Contains(recorder.sql, "LIKE") {
		t.Fatalf("空关键字不应产生 LIKE 谓词: %s", recorder.sql)
	}
	if strings.Contains(recorder.sql, "id > ?") {
		t.Fatalf("空游标不应产生 id > 谓词: %s", recorder.sql)
	}
	clamped := false
	for _, value := range recorder.params {
		if limit, ok := value.(int); ok && limit == 500 {
			clamped = true
		}
	}
	if !clamped {
		t.Fatalf("limit 必须继续钳到 500: %v", recorder.params)
	}
}

// TestListApprovedContractReferencesScopedKeywordKeepsResponsibilityScope 固定责任范围：
// 关键字只收窄结果，owner_identity_id/tenant_id 谓词必须保留，游标仍按 id 升序。
func TestListApprovedContractReferencesScopedKeywordKeepsResponsibilityScope(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
	if _, err := repository.ListApprovedContractReferencesScoped(context.Background(), filter, "HT-2", "C-9", 50); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{
		"tenant_id = ?", "owner_identity_id = ?",
		"contract_number LIKE ?", "title LIKE ?", "customer_name LIKE ?",
		"COALESCE(NULLIF(origin_kind,''),'NORMAL')='NORMAL'", "id > ?", "ORDER BY id ASC",
	} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("范围引用查询缺少 %q: %s", fragment, recorder.sql)
		}
	}
	patterns := boundKeywordPatterns(recorder.params)
	if len(patterns) != 3 || patterns[0] != "%HT-2%" {
		t.Fatalf("范围关键字模式=%v，want 三个 HT-2 模式", patterns)
	}
}
