package mysql

import (
	"context"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestContractLedgerFiltersApplyKeywordStatusAndType 固定台账谓词口径：
// 关键字三列匹配、状态与类型都是等值过滤，三者可以叠加；未给出的条件不产生谓词。
func TestContractLedgerFiltersApplyKeywordStatusAndType(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	query := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{
		Keyword: "HT-2026", Status: "active", Type: "直签",
	})
	if err := query.Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{
		"contract_number LIKE ?", "title LIKE ?", "customer_name LIKE ?",
		"status = ?", "contract_type = ?",
	} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("台账谓词缺少 %q: %s", fragment, recorder.sql)
		}
	}
	// 绑定顺序必须是 keyword 三个模式 → status → type，避免参数错位把类型当状态查。
	want := []string{"%HT-2026%", "%HT-2026%", "%HT-2026%", "active", "直签"}
	if len(recorder.params) != len(want) {
		t.Fatalf("绑定参数 = %#v，want %#v", recorder.params, want)
	}
	for index := range want {
		if recorder.params[index] != want[index] {
			t.Fatalf("第 %d 个绑定参数 = %#v，want %q", index, recorder.params[index], want[index])
		}
	}
}

// TestContractLedgerFiltersEscapesKeywordMetacharacters 固定台账关键字转义口径：
// 用户输入里的 % _ \ 必须按字面量绑定（MySQL LIKE 默认以反斜杠为转义符，
// 因此 SQL 里不写 ESCAPE 子句，转义全部体现在绑定参数中），
// 否则 "100%" / "A_B" 会退化成通配符，命中行数和 total 都被放大。
func TestContractLedgerFiltersEscapesKeywordMetacharacters(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	query := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{
		Keyword: " 100%_a\\b ", Status: "active", Type: "直签",
	})
	if err := query.Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{
		"contract_number LIKE ?", "title LIKE ?", "customer_name LIKE ?",
		"status = ?", "contract_type = ?",
	} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("台账谓词缺少 %q: %s", fragment, recorder.sql)
		}
	}
	// 转义口径必须与已批准合同引用查询完全一致，不能出现第二套方言。
	want := literalLikePattern("100%_a\\b")
	if want != `%100\%\_a\\b%` {
		t.Fatalf("转义模式 = %q，want %q", want, `%100\%\_a\\b%`)
	}
	wantParams := []any{want, want, want, "active", "直签"}
	if len(recorder.params) != len(wantParams) {
		t.Fatalf("绑定参数 = %#v，want %#v", recorder.params, wantParams)
	}
	for index, expected := range wantParams {
		if recorder.params[index] != expected {
			t.Fatalf("第 %d 个绑定参数 = %#v，want %#v", index, recorder.params[index], expected)
		}
	}
	// 反向验证：未转义模式（把 % 和 _ 当通配符）绝不能出现。
	if containsParam(recorder.params, "%100%_a\\b%") {
		t.Fatalf("关键字模式未转义: %#v", recorder.params)
	}
	// 转义符固定为反斜杠（MySQL LIKE 默认），SQL 不额外声明 ESCAPE。
	if strings.Contains(strings.ToUpper(recorder.sql), "ESCAPE") {
		t.Fatalf("SQL 不应声明 ESCAPE 子句（依赖 MySQL 默认反斜杠转义）: %s", recorder.sql)
	}
}

// TestContractLedgerFiltersPlainKeywordBindsAsBefore 固定无元字符关键字的现状：
// 普通关键字（合同编号片段）仍绑定成 %keyword%，与修复前逐字节相同，没有回归。
func TestContractLedgerFiltersPlainKeywordBindsAsBefore(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	query := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{Keyword: "HT-2026-001"})
	if err := query.Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	patterns := boundKeywordPatterns(recorder.params)
	if len(patterns) != 3 {
		t.Fatalf("关键字应绑定 3 个 LIKE 模式，实际 %d 个: %#v", len(patterns), recorder.params)
	}
	for _, pattern := range patterns {
		if pattern != "%HT-2026-001%" {
			t.Fatalf("普通关键字模式 = %q，want %q", pattern, "%HT-2026-001%")
		}
	}
}

// TestListContractsPageScopedCountAndPageShareEscapedKeyword 固定分页台账两条语句同源：
// COUNT 与取页都由 contractLedgerFilters 构造，带同样的 LIMIT 转义模式，
// 只给取页转义会让 total 仍按通配符放大，"100%" 的总数与页内行数不一致。
func TestListContractsPageScopedCountAndPageShareEscapedKeyword(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
	if _, err := repository.ListContractsPageScoped(context.Background(), filter, contract.ContractListQuery{
		Keyword: "100%_a\\b", Offset: 0, Limit: 20,
	}); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	want := `%100\%\_a\\b%`
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
		for _, fragment := range []string{"contract_number LIKE ?", "title LIKE ?", "customer_name LIKE ?"} {
			if !strings.Contains(statement, fragment) {
				t.Fatalf("%s 语句缺少关键字谓词 %q: %s", name, fragment, statement)
			}
		}
	}
	for name, params := range map[string][]any{"COUNT": countParams, "取页": pageParams} {
		patterns := boundKeywordPatterns(params)
		if len(patterns) != 3 {
			t.Fatalf("%s 语句应绑定 3 个关键字模式，实际 %d 个: %#v", name, len(patterns), params)
		}
		for _, pattern := range patterns {
			if pattern != want {
				t.Fatalf("%s 语句关键字模式 = %q，want %q", name, pattern, want)
			}
		}
	}
}

// TestContractLedgerFiltersOmitsEmptyConditions 固定空条件行为：
// 不传类型/状态/关键字时不追加任何谓词（旧调用方的 SQL 形状不变）。
func TestContractLedgerFiltersOmitsEmptyConditions(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	var records []contractRecord
	if err := contractLedgerFilters(db.Model(&contractRecord{}), contract.ContractListQuery{}).Find(&records).Error; err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{"LIKE", "status = ?", "contract_type = ?"} {
		if strings.Contains(recorder.sql, fragment) {
			t.Fatalf("空条件不应产生 %q: %s", fragment, recorder.sql)
		}
	}
}

// TestListContractsPageScopedAppliesScopeTypeAndPaging 固定生产查询：
// 数据范围先行收窄，类型筛选、稳定排序（updated_at DESC, id DESC）以及 LIMIT/OFFSET 同时在 SQL 里，
// 并且不读取渲染文档大字段。
func TestListContractsPageScopedAppliesScopeTypeAndPaging(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
	page, err := repository.ListContractsPageScoped(context.Background(), filter, contract.ContractListQuery{
		Keyword: "HT-2", Status: "active", Type: "三方", Offset: 20, Limit: 20,
	})
	if err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{
		"tenant_id = ?", "owner_identity_id = ?",
		"contract_number LIKE ?", "status = ?", "contract_type = ?",
		"ORDER BY updated_at DESC, id DESC", "LIMIT ?", "OFFSET ?",
	} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("分页台账查询缺少 %q: %s", fragment, recorder.sql)
		}
	}
	if strings.Contains(recorder.sql, "rendered_document") || strings.Contains(recorder.sql, "template_values_json") {
		t.Fatalf("分页台账查询不应读取大字段: %s", recorder.sql)
	}
	if !containsParam(recorder.params, "三方") {
		t.Fatalf("类型筛选未绑定: %#v", recorder.params)
	}
	if !containsParam(recorder.params, 20) {
		t.Fatalf("limit/offset 未绑定: %#v", recorder.params)
	}
	if page.Page != 2 || page.PageSize != 20 {
		t.Fatalf("分页信封 = page %d page_size %d，want 2/20", page.Page, page.PageSize)
	}
}

// TestListContractsPageScopedCountAlsoNarrowedByScope 固定“先收窄再计数”：
// 分页台账是 COUNT + 取页两条独立语句，两条都必须带租户/责任范围；
// 只校验取页语句会让“跨租户 total 泄漏”这类缺陷在测试里完全看不见。
func TestListContractsPageScopedCountAlsoNarrowedByScope(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
	if _, err := repository.ListContractsPageScoped(context.Background(), filter, contract.ContractListQuery{Offset: 0, Limit: 20}); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	if len(recorder.sqls) < 2 {
		t.Fatalf("分页台账应生成 COUNT 与取页两条语句，实际 %d 条: %#v", len(recorder.sqls), recorder.sqls)
	}
	var countSQL, pageSQL string
	for _, statement := range recorder.sqls {
		if strings.Contains(strings.ToUpper(statement), "COUNT(") {
			countSQL = statement
		}
		if strings.Contains(strings.ToUpper(statement), "LIMIT") {
			pageSQL = statement
		}
	}
	if countSQL == "" || pageSQL == "" {
		t.Fatalf("未同时找到 COUNT 与取页语句: %#v", recorder.sqls)
	}
	for name, statement := range map[string]string{"COUNT": countSQL, "取页": pageSQL} {
		for _, fragment := range []string{"tenant_id = ?", "owner_identity_id = ?"} {
			if !strings.Contains(statement, fragment) {
				t.Fatalf("%s 语句缺少数据范围谓词 %q: %s", name, fragment, statement)
			}
		}
	}
	if strings.Contains(countSQL, "LIMIT") {
		t.Fatalf("COUNT 语句不应带分页上限: %s", countSQL)
	}
}

// TestListContractsLegacyScopedSharesTypeAwareFilters 固定旧方法同源：
// ListContractsScoped 使用同一套谓词构造（含稳定排序），保证类型筛选口径不分叉。
func TestListContractsLegacyScopedSharesTypeAwareFilters(t *testing.T) {
	db, recorder := newApprovedContractDryRun(t)
	repository := &Repository{db: db}
	filter := contract.ScopeFilter{TenantID: "tenant-1", AllowAll: true}
	if _, err := repository.ListContractsScoped(context.Background(), filter, "", "active", 50); err != nil {
		t.Fatalf("dry-run query: %v", err)
	}
	for _, fragment := range []string{"tenant_id = ?", "status = ?", "ORDER BY updated_at DESC, id DESC", "LIMIT ?"} {
		if !strings.Contains(recorder.sql, fragment) {
			t.Fatalf("旧范围查询缺少 %q: %s", fragment, recorder.sql)
		}
	}
	if strings.Contains(recorder.sql, "OFFSET") {
		t.Fatalf("旧范围查询不应带 OFFSET: %s", recorder.sql)
	}
	if strings.Contains(recorder.sql, "contract_type = ?") {
		t.Fatalf("未传类型时不应产生类型谓词: %s", recorder.sql)
	}
}

// containsParam 判断绑定参数里是否出现期望值（int 与 int64 都按数值比较）。
func containsParam(params []any, want any) bool {
	for _, value := range params {
		switch expected := want.(type) {
		case string:
			if text, ok := value.(string); ok && text == expected {
				return true
			}
		case int:
			switch actual := value.(type) {
			case int:
				if actual == expected {
					return true
				}
			case int64:
				if actual == int64(expected) {
					return true
				}
			}
		}
	}
	return false
}
