package mysql

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// ledgerBusinessToday 是台账过期谓词绑定的“今天”：业务日历日（北京）对应的
// 「同日 00:00 UTC 墙钟」，与 contracts_signing_query.go 的写法同源。
func ledgerBusinessToday() time.Time { return businessclock.CalendarDay(businessclock.Now()) }

// boundStatusParams 把 SQL 绑定参数规整成字符串，便于断言 IN 展开后的状态值
// （切片展开后每个元素是 contract.Status，直接与 string 比较会因动态类型不同而失败）。
func boundStatusParams(params []any) []string {
	values := make([]string, 0, len(params))
	for _, param := range params {
		values = append(values, fmt.Sprint(param))
	}
	return values
}

// TestContractLedgerFiltersExpiredTrueAddsStatusAndDatePredicate 固定 expired=true 口径：
// 参与过期判定的状态（生效/执行中/待收款）且到期日严格早于北京今天；
// 到期日当天不算过期，因此谓词必须是 `<` 而不是 `<=`。
func TestContractLedgerFiltersExpiredTrueAddsStatusAndDatePredicate(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	if err := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{
		Expired: "true",
	}).Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{"status IN (", "end_date IS NOT NULL", "end_date < ?"} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("expired=true 缺少 %q: %s", fragment, recorder.sql)
		}
	}
	if strings.Contains(recorder.sql, "status NOT IN (") {
		t.Fatalf("expired=true 不应出现否定谓词: %s", recorder.sql)
	}
	want := []string{"active", "in_progress", "pending_pay"}
	got := boundStatusParams(recorder.params)
	if len(got) != len(want)+1 {
		t.Fatalf("绑定参数 = %#v，want 3 个状态 + 1 个日期", recorder.params)
	}
	for index, status := range want {
		if got[index] != status {
			t.Fatalf("第 %d 个绑定状态 = %q，want %q", index, got[index], status)
		}
	}
	today, ok := recorder.params[len(recorder.params)-1].(time.Time)
	if !ok {
		t.Fatalf("日期参数类型 = %T，want time.Time", recorder.params[len(recorder.params)-1])
	}
	if !today.Equal(ledgerBusinessToday()) {
		t.Fatalf("绑定的今天 = %v，want 业务日历日 %v", today, ledgerBusinessToday())
	}
	if today.Location() != time.UTC {
		t.Fatalf("今天的时区 = %v，want UTC 墙钟（DATE 列按 UTC 墙钟比较）", today.Location())
	}
}

// TestContractLedgerFiltersExpiredFalseNegatesTheRule 固定 expired=false 口径：
// 它是「已过期」的否定，而不是 status != active；到期日为空的合同视为未过期
// （与前端 isContractExpired 缺 end_date 不判过期一致）。
func TestContractLedgerFiltersExpiredFalseNegatesTheRule(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	if err := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{
		Expired: "false",
	}).Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{"status NOT IN (", "end_date IS NULL", "end_date >= ?"} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("expired=false 缺少 %q: %s", fragment, recorder.sql)
		}
	}
	// 否定式必须是「结束日不早于今天」的显式析取，不能出现 `<`（否则空值语义错乱）。
	if strings.Contains(recorder.sql, "end_date < ?") {
		t.Fatalf("expired=false 不应包含“早于今天”的谓词: %s", recorder.sql)
	}
	want := []string{"active", "in_progress", "pending_pay"}
	got := boundStatusParams(recorder.params)
	if len(got) != len(want)+1 {
		t.Fatalf("绑定参数 = %#v，want 3 个状态 + 1 个日期", recorder.params)
	}
	for index, status := range want {
		if got[index] != status {
			t.Fatalf("第 %d 个绑定状态 = %q，want %q", index, got[index], status)
		}
	}
	today, ok := recorder.params[len(recorder.params)-1].(time.Time)
	if !ok || !today.Equal(ledgerBusinessToday()) {
		t.Fatalf("绑定的今天 = %#v，want 业务日历日 %v", recorder.params[len(recorder.params)-1], ledgerBusinessToday())
	}
}

// TestContractLedgerFiltersIgnoresInvalidExpiredValue 固定宽松口径：
// 只接受 true/false（大小写不敏感、忽略两端空白），其它取值退化为“不过滤”，
// 既不返回 422 也不追加任何谓词。
func TestContractLedgerFiltersIgnoresInvalidExpiredValue(t *testing.T) {
	for _, value := range []string{"", "yes", "1", "0", "过期", "truee", "null"} {
		db, recorder := newApprovedContractDryRun(t)
		var records []contractRecord
		if err := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{
			Expired: value,
		}).Find(&records).Error; err != nil {
			t.Fatalf("%q: dry-run query: %v", value, err)
		}
		for _, fragment := range []string{"end_date", "status IN (", "status NOT IN ("} {
			if strings.Contains(recorder.sql, fragment) {
				t.Fatalf("%q 应退化为不过滤，却产生了 %q: %s", value, fragment, recorder.sql)
			}
		}
	}
}

// TestContractLedgerFiltersBindOrderWithExpired 固定绑定顺序：
// keyword ×3 → status → type → expired（3 个状态 → 今天）。
// 顺序错位会把类型当状态、把日期当 LIMIT 绑定，测试必须逐位锁定。
func TestContractLedgerFiltersBindOrderWithExpired(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	if err := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{
		Keyword: "HT-2026", Status: "approved", Type: "直签", Expired: " true ",
	}).Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{
		"contract_number LIKE ?", "title LIKE ?", "customer_name LIKE ?",
		"status = ?", "contract_type = ?", "status IN (", "end_date < ?",
	} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("组合筛选缺少 %q: %s", fragment, recorder.sql)
		}
	}
	pattern := literalLikePattern("HT-2026")
	want := []string{
		pattern, pattern, pattern, "approved", "直签",
		"active", "in_progress", "pending_pay",
	}
	got := boundStatusParams(recorder.params)
	if len(got) != len(want)+1 {
		t.Fatalf("绑定参数 = %#v，want %d 个筛选值 + 今天", recorder.params, len(want))
	}
	for index, expected := range want {
		if got[index] != expected {
			t.Fatalf("第 %d 个绑定参数 = %q，want %q（顺序 keyword×3 → status → type → expired）", index, got[index], expected)
		}
	}
	today, ok := recorder.params[len(want)].(time.Time)
	if !ok || !today.Equal(ledgerBusinessToday()) {
		t.Fatalf("最后一个绑定参数 = %#v，want 业务日历日 %v", recorder.params[len(want)], ledgerBusinessToday())
	}
}

// TestListContractsPageScopedCountAndPageShareExpiredPredicate 固定分页台账两条语句同源：
// COUNT 与取页都由 contractLedgerFilters 构造，过期谓词与“今天”参数必须同时出现在两条里，
// 否则 total 与页内行数会分叉（页面显示“共 N 条”但翻页缺行）。
func TestListContractsPageScopedCountAndPageShareExpiredPredicate(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
	if _, err := repository.ListContractsPageScoped(context.Background(), filter, contract.ContractListQuery{
		Expired: "true", Offset: 0, Limit: 20,
	}); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	var countSQL, pageSQL string
	var countParams, pageParams []any
	for index, statement := range recorder.sqls {
		switch {
		case strings.Contains(strings.ToUpper(statement), "COUNT("):
			countSQL, countParams = statement, recorder.paramSets[index]
		case strings.Contains(strings.ToUpper(statement), "LIMIT"):
			pageSQL, pageParams = statement, recorder.paramSets[index]
		}
	}
	if countSQL == "" || pageSQL == "" {
		t.Fatalf("未同时找到 COUNT 与取页语句: %#v", recorder.sqls)
	}
	for name, statement := range map[string]string{"COUNT": countSQL, "取页": pageSQL} {
		for _, fragment := range []string{"status IN (", "end_date IS NOT NULL", "end_date < ?"} {
			if !strings.Contains(statement, fragment) {
				t.Fatalf("%s 语句缺少过期谓词 %q: %s", name, fragment, statement)
			}
		}
	}
	for name, params := range map[string][]any{"COUNT": countParams, "取页": pageParams} {
		// 前缀是数据范围参数（tenant_id、owner_identity_id），取页语句尾部还有 LIMIT/OFFSET，
		// 因此以「业务日历日」这个 time.Time 参数为锚点，断言它前面紧跟 3 个过期状态。
		todayIndex := -1
		for index, param := range params {
			if _, ok := param.(time.Time); ok {
				todayIndex = index
				break
			}
		}
		if todayIndex < 3 {
			t.Fatalf("%s 绑定参数 = %#v，未找到“今天”及其前面 3 个过期状态", name, params)
		}
		got := boundStatusParams(params[todayIndex-3 : todayIndex])
		for index, status := range []string{"active", "in_progress", "pending_pay"} {
			if got[index] != status {
				t.Fatalf("%s 第 %d 个过期状态 = %q，want %q", name, index, got[index], status)
			}
		}
		today := params[todayIndex].(time.Time)
		if !today.Equal(ledgerBusinessToday()) {
			t.Fatalf("%s 的今天参数 = %v，want 业务日历日 %v", name, today, ledgerBusinessToday())
		}
	}
}
