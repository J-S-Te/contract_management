package mysql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
)

// likeKeywordSite 描述一个接收用户关键字的 LIKE 入口。
// run 用给定关键字驱动站点自身的生产查询；fragment/paramCount 用于在站点生成的多条语句里
// 精确定位带关键字的语句（签署台账的 COUNT 与取页语句同形，必须逐条检查，只看最后一条
// 会让其中一条悄悄退回裸拼接而测试仍然通过）。
type likeKeywordSite struct {
	name       string
	fragment   string
	paramCount int
	run        func(t *testing.T, db *gorm.DB, keyword string) error
}

// userKeywordLikeSites 枚举全部“用户关键字 → SQL LIKE”入口。
// 本清单与 literalLikePattern 的调用点一一对应：新增入口时必须同时加到这里，
// 否则未转义的站点不会被任何用例覆盖。
//   - 签署台账：applySigningRecordFilters（c./s. 四列）
//   - 结算消费合同：ListIntegrationCompletedContractsScoped
//   - 本地目录 owner 列表：ListLocalCounterparties / ListLocalCategories
//   - 授权可见目录：ListVisibleLocalCounterparties / ListVisibleLocalCategories
//
// 合同台账（contractLedgerFilters）与已批准引用（applyApprovedContractKeyword）
// 已由 contracts_ledger_page_test.go / contracts_approved_keyword_test.go 覆盖，不重复列。
var userKeywordLikeSites = []likeKeywordSite{
	{
		name:       "签署台账",
		fragment:   "(c.contract_number LIKE ? OR c.title LIKE ? OR c.customer_name LIKE ? OR s.courier_number LIKE ?)",
		paramCount: 4,
		// 与 ListSigningRecordsScoped 的取页语句同构：先 left join 签署行，
		// 再套同一份筛选构造，避免依赖 total>0 的分支。
		run: func(t *testing.T, db *gorm.DB, keyword string) error {
			t.Helper()
			filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
			query := applySigningRecordFilters(signingLedgerQuery(db, filter), contract.SigningRecordQuery{Keyword: keyword, Limit: 20}, time.Time{})
			var ids []string
			return query.Select("c.id").Order(signingLedgerOrder).Offset(0).Limit(20).Pluck("c.id", &ids).Error
		},
	},
	{
		name:       "结算消费合同列表",
		fragment:   "(contract_number LIKE ? OR title LIKE ? OR customer_name LIKE ?)",
		paramCount: 3,
		run: func(t *testing.T, db *gorm.DB, keyword string) error {
			t.Helper()
			repository := &Repository{db: db}
			filter := contract.ScopeFilter{TenantID: "tenant-1", IdentityID: "identity-1", AllowSelf: true}
			_, err := repository.ListIntegrationCompletedContractsScoped(context.Background(), filter, keyword, 50)
			return err
		},
	},
	{
		name:       "本地目录交易对方",
		fragment:   "name LIKE ?",
		paramCount: 1,
		run: func(t *testing.T, db *gorm.DB, keyword string) error {
			t.Helper()
			repository := &Repository{db: db}
			_, err := repository.ListLocalCounterparties(context.Background(), contract.ScopeFilter{TenantID: "tenant-1", AllowAll: true}, keyword, 50)
			return err
		},
	},
	{
		name:       "本地目录类别",
		fragment:   "(name LIKE ? OR code LIKE ?)",
		paramCount: 2,
		run: func(t *testing.T, db *gorm.DB, keyword string) error {
			t.Helper()
			repository := &Repository{db: db}
			_, err := repository.ListLocalCategories(context.Background(), contract.ScopeFilter{TenantID: "tenant-1", AllowAll: true}, keyword, 50)
			return err
		},
	},
	{
		name:       "授权可见交易对方",
		fragment:   "name LIKE ?",
		paramCount: 1,
		run: func(t *testing.T, db *gorm.DB, keyword string) error {
			t.Helper()
			repository := &Repository{db: db}
			visibility := contract.LocalCatalogVisibility{IdentityID: "identity-1"}
			_, err := repository.ListVisibleLocalCounterparties(context.Background(), "tenant-1", visibility, keyword, 50)
			return err
		},
	},
	{
		name:       "授权可见类别",
		fragment:   "(name LIKE ? OR code LIKE ?)",
		paramCount: 2,
		run: func(t *testing.T, db *gorm.DB, keyword string) error {
			t.Helper()
			repository := &Repository{db: db}
			visibility := contract.LocalCatalogVisibility{IdentityID: "identity-1"}
			_, err := repository.ListVisibleLocalCategories(context.Background(), "tenant-1", visibility, keyword, 50)
			return err
		},
	},
}

// TestUserKeywordLikeSitesEscapeMetacharacters 固定所有用户关键字入口共用一套转义口径：
// 关键字里的 % _ \ 必须按字面量绑定（MySQL LIKE 默认以反斜杠为转义符，故 SQL 不写
// ESCAPE 子句），否则 "100%_a\b" 会退化成通配符，命中行数、total 与分页一起失真。
func TestUserKeywordLikeSitesEscapeMetacharacters(t *testing.T) {
	const keyword = "100%_a\\b"
	want := `%100\%\_a\\b%`
	// 先钉住转义方言本身，避免 helper 与断言同时漂移导致两边一起错。
	if got := literalLikePattern(keyword); got != want {
		t.Fatalf("literalLikePattern(%q) = %q，want %q", keyword, got, want)
	}
	for _, site := range userKeywordLikeSites {
		t.Run(site.name, func(t *testing.T) {
			db, recorder := newApprovedContractDryRun(t)
			if err := site.run(t, db, keyword); err != nil {
				t.Fatalf("dry-run query: %v", err)
			}
			assertEscapedLikeBindings(t, recorder, site.fragment, site.paramCount, want)
			// 反向验证：未转义模式（把 % 和 _ 当通配符）绝不能出现在任何绑定参数里。
			if containsParam(recorder.params, "%100%_a\\b%") {
				t.Fatalf("关键字模式未转义: %#v", recorder.params)
			}
		})
	}
}

// TestUserKeywordLikeSitesBindPlainKeywordAsBefore 固定无元字符关键字的现状：
// 普通关键字仍绑定成 %keyword%，与修复前逐字节相同，列表结果、排序与分页不变。
func TestUserKeywordLikeSitesBindPlainKeywordAsBefore(t *testing.T) {
	const keyword = "HT-2026-001"
	const want = "%HT-2026-001%"
	for _, site := range userKeywordLikeSites {
		t.Run(site.name, func(t *testing.T) {
			db, recorder := newApprovedContractDryRun(t)
			if err := site.run(t, db, keyword); err != nil {
				t.Fatalf("dry-run query: %v", err)
			}
			assertEscapedLikeBindings(t, recorder, site.fragment, site.paramCount, want)
		})
	}
}

// assertEscapedLikeBindings 断言站点生成的每条带关键字谓词的语句都只绑定期望的字面量模式。
// 参数：recorder 为 DryRun 捕获的语句与绑定参数，fragment 为站点专属的 LIKE 谓词片段，
// wantCount 为该站点绑定的模式个数（多列 OR 时为列数），want 为期望模式。
// 返回值：无；不符即 fail。同时确认 SQL 不声明 ESCAPE 子句，转义口径只依赖绑定参数。
func assertEscapedLikeBindings(t *testing.T, recorder *approvedContractSQLRecorder, fragment string, wantCount int, want string) {
	t.Helper()
	matched := 0
	for index, statement := range recorder.sqls {
		if !strings.Contains(statement, fragment) {
			continue
		}
		matched++
		if strings.Contains(strings.ToUpper(statement), "ESCAPE") {
			t.Fatalf("SQL 不应声明 ESCAPE 子句（依赖 MySQL 默认反斜杠转义）: %s", statement)
		}
		patterns := boundKeywordPatterns(recorder.paramSets[index])
		if len(patterns) != wantCount {
			t.Fatalf("语句应绑定 %d 个 LIKE 模式，实际 %d 个: %#v（语句: %s）", wantCount, len(patterns), recorder.paramSets[index], statement)
		}
		for _, pattern := range patterns {
			if pattern != want {
				t.Fatalf("LIKE 模式 = %q，want %q（语句: %s）", pattern, want, statement)
			}
		}
	}
	if matched == 0 {
		t.Fatalf("未生成带 %q 的语句: %#v", fragment, recorder.sqls)
	}
}
