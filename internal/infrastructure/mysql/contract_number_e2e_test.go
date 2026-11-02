package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// saveContractNumberRule 写入测试规则；规则行是全局单行配置，用例必须显式覆盖。
func saveContractNumberRule(t *testing.T, r *Repository, rule contract.ContractNumberRule) {
	t.Helper()
	normalized, err := contract.NormalizeContractNumberRule(rule)
	if err != nil {
		t.Fatalf("normalize rule: %v", err)
	}
	normalized.UpdatedBy = "e2e"
	normalized.UpdatedAt = time.Now().UTC()
	if err := r.SaveContractNumberRule(context.Background(), normalized); err != nil {
		t.Fatalf("save rule: %v", err)
	}
}

// restoreDefaultContractNumberRule 在用例结束后恢复迁移种子默认规则，
// 避免全局单行配置影响同包后续的审批完成用例。
func restoreDefaultContractNumberRule(t *testing.T, r *Repository) {
	t.Helper()
	t.Cleanup(func() {
		rule := contract.DefaultContractNumberRule()
		rule.UpdatedBy = "e2e"
		rule.UpdatedAt = time.Now().UTC()
		if err := r.SaveContractNumberRule(context.Background(), rule); err != nil {
			t.Errorf("restore default contract number rule: %v", err)
		}
	})
}

// contractNumberRuleKey 返回测试规则的流水桶指纹，用例据此清理与断水水位。
func contractNumberRuleKey(t *testing.T, rule contract.ContractNumberRule) string {
	t.Helper()
	key, err := contract.ContractNumberRuleKey(rule)
	if err != nil {
		t.Fatalf("rule key: %v", err)
	}
	return key
}

// resetContractNumberSequence 清空用例专属规则桶的全系统流水。
//
// 流水桶只按 (年度, 规则指纹) 分库计数、不区分租户，因此同一个隔离库上重复执行本
// 用例时必须显式回到零水位；清理范围严格限定为本用例用到的规则指纹，
// 不触碰其他用例（默认规则 HT- 等）的桶。
func resetContractNumberSequence(t *testing.T, r *Repository, rules ...contract.ContractNumberRule) {
	t.Helper()
	keys := make([]string, 0, len(rules))
	for _, rule := range rules {
		keys = append(keys, contractNumberRuleKey(t, rule))
	}
	if len(keys) == 0 {
		return
	}
	if err := r.db.Where("rule_key IN ?", keys).Delete(&contractNumberSequenceRecord{}).Error; err != nil {
		t.Fatalf("reset contract number sequence: %v", err)
	}
}

// contractNumberTestNumberPatterns 是本包用例专用的单号前缀（GTSR-/ABCD-），
// 只有 contract_number_e2e_test.go 会生成这两个前缀的编号。
var contractNumberTestNumberPatterns = []string{"GTSR-%", "ABCD-%"}

// resetContractNumberRegistry 清理本包用例专用前缀在全局登记表里的历史预占。
//
// 登记表主键只有 contract_number，不含租户，不能像普通业务表那样按 tenant_id 清理；
// 而用例会故意用随机租户取号，那些租户不会被 draftE2ERepository 的清理列表覆盖。
// 因此这里严格按本包专用单号前缀清理，重复执行同一隔离库时不会互相污染，
// 也不影响其他用例（如默认规则 HT- 的审批完成用例）。
func resetContractNumberRegistry(t *testing.T, r *Repository) {
	t.Helper()
	for _, pattern := range contractNumberTestNumberPatterns {
		if err := r.db.Where("contract_number LIKE ?", pattern).Delete(&contractNumberRegistryRecord{}).Error; err != nil {
			t.Fatalf("reset contract number registry: %v", err)
		}
	}
}

// resetContractNumberState 把用例用到的流水桶与登记表前缀一次清干净。
func resetContractNumberState(t *testing.T, r *Repository, rules ...contract.ContractNumberRule) {
	t.Helper()
	resetContractNumberSequence(t, r, rules...)
	resetContractNumberRegistry(t, r)
}

// allocateInIndependentTx 在独立事务内执行审批同款取号逻辑。
func allocateInIndependentTx(r *Repository, tenantID, legacyFormat string, now time.Time) (string, error) {
	var number string
	err := r.db.Transaction(func(tx *gorm.DB) error {
		allocated, err := allocateContractNumberInTx(tx, tenantID, ulid.Make().String(), legacyFormat, now)
		if err != nil {
			return err
		}
		number = allocated
		return nil
	})
	return number, err
}

// seedContractNumberHolder 占位一条已签发过的历史合同编号，验证取号顺延。
//
// 新实现下“历史已签发”的事实记在全局登记表里，因此这里同时写合同行和登记表行：
// 只写合同行不会阻止取号（占用判定只看登记表）。seq_year/rule_key 留空表示
// 该号来自旧规则或模板格式，无法反推规则桶——正是跨租户历史重号的场景。
func seedContractNumberHolder(t *testing.T, r *Repository, tenantID, number string) {
	t.Helper()
	now := time.Now().UTC()
	record := contractRecord{
		ID: ulid.Make().String(), TenantID: tenantID, ContractNumber: &number, Title: "编号占用测试",
		ContractType: "sales", ServiceType: "integration", OwnerUserID: ulid.Make().String(),
		AmountMinor: 100, Currency: "CNY", Content: "{}", Status: string(contract.StatusDraft),
		Version: 1, CreatedAt: now, CreatedBy: "seed", UpdatedAt: now, UpdatedBy: "seed",
	}
	if err := r.db.Create(&record).Error; err != nil {
		t.Fatalf("seed contract number holder: %v", err)
	}
	registry := contractNumberRegistryRecord{
		ContractNumber: number, TenantID: tenantID, ContractID: record.ID, IssuedAt: now,
	}
	if err := r.db.Create(&registry).Error; err != nil {
		t.Fatalf("seed contract number registry: %v", err)
	}
	// 用例会故意在“别的租户”名下占号，该租户不在 draftE2ERepository 的清理范围内，
	// 这里补一条按租户精确限定的清理，避免同一隔离库重复执行时留下脏合同行。
	t.Cleanup(func() {
		if err := r.db.Where("tenant_id = ?", tenantID).Delete(&contractNumberRegistryRecord{}).Error; err != nil {
			t.Errorf("seeded registry cleanup failed")
		}
		if err := r.db.Where("tenant_id = ?", tenantID).Delete(&contractRecord{}).Error; err != nil {
			t.Errorf("seeded contract cleanup failed")
		}
	})
}

// TestContractNumberRuleRoundTripOnIsolatedMySQL 验证全局单行规则的读写。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberRuleRoundTripOnIsolatedMySQL(t *testing.T) {
	r, _ := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	ctx := context.Background()
	saveContractNumberRule(t, r, contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4, Suffix: "/A"})
	rule, found, err := r.GetContractNumberRule(ctx)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if rule.Prefix != "GTSR-" || rule.YearSegment != contract.NumberYearSegmentYY || rule.SeqWidth != 4 ||
		rule.Suffix != "/A" || rule.UpdatedBy != "e2e" || rule.UpdatedAt.IsZero() {
		t.Fatalf("rule=%+v", rule)
	}
	// 迁移种子行必须被覆盖而不是新增第二行。
	var rows int64
	if err := r.db.Model(&contractNumberRuleRecord{}).Count(&rows).Error; err != nil || rows != 1 {
		t.Fatalf("rule rows=%d err=%v", rows, err)
	}
}

// TestContractNumberSequenceSharedAcrossTenantsOnIsolatedMySQL 验证流水桶全系统共享：
// 两个不同租户使用同一规则时接着同一个计数器取号，不会各自从 1 开始。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberSequenceSharedAcrossTenantsOnIsolatedMySQL(t *testing.T) {
	r, tenantA := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	key := contractNumberRuleKey(t, rule)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	tenantB := ulid.Make().String()

	if number, err := allocateInIndependentTx(r, tenantA, "", now); err != nil || number != "GTSR-260001" {
		t.Fatalf("租户 A 首次取号=%q err=%v want GTSR-260001", number, err)
	}
	// 租户 B 不得重号，必须接着全局计数器递增。
	if number, err := allocateInIndependentTx(r, tenantB, "", now); err != nil || number != "GTSR-260002" {
		t.Fatalf("租户 B 取号=%q err=%v want GTSR-260002", number, err)
	}
	// 回到租户 A 继续取号，仍是同一个全系统计数器。
	if number, err := allocateInIndependentTx(r, tenantA, "", now); err != nil || number != "GTSR-260003" {
		t.Fatalf("租户 A 再次取号=%q err=%v want GTSR-260003", number, err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2026); err != nil || current != 3 {
		t.Fatalf("全系统水位=%d err=%v want 3", current, err)
	}

	// 跨租户唯一性由全局登记表主键强制：两个租户的实际编号集合互不重叠。
	numbers := map[string]bool{"GTSR-260001": true}
	for index := 0; index < 3; index++ {
		number, err := allocateInIndependentTx(r, ulid.Make().String(), "", now)
		if err != nil {
			t.Fatalf("新租户取号 %d: %v", index, err)
		}
		if numbers[number] {
			t.Fatalf("跨租户出现重号: %s", number)
		}
		numbers[number] = true
	}
}

// TestContractNumberYearSegmentFollowsBeijingCalendarOnIsolatedMySQL 验证单号年份段、年度
// 流水桶与登记行都按北京日历日取年，而不是按 UTC 取年：北京时间每天 00:00–08:00 恰好
// 落在 UTC 的前一天，这段区间签发的合同若按 UTC 取年，会把年份段、seq_year（年度流水桶）
// 和登记行年度整体写错一年——写错数据，不只是显示问题。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberYearSegmentFollowsBeijingCalendarOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	t.Cleanup(func() { resetContractNumberSequence(t, r, rule) })
	key := contractNumberRuleKey(t, rule)
	ctx := context.Background()

	// ① UTC 2025-12-31T15:30 = 北京 2025-12-31 23:30：仍在 2025 年桶里。
	lastMomentOfBeijing2025 := time.Date(2025, 12, 31, 15, 30, 0, 0, time.UTC)
	// ② UTC 2025-12-31T16:30 = 北京 2026-01-01 00:30：UTC 还停在 2025-12-31，但业务上
	//    已经是 2026 年，必须落到 2026 年份段与 2026 年桶（旧实现会错写成 25/GTSR-250002）。
	firstMomentOfBeijing2026 := time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC)
	// ③ UTC 2026-01-01T03:00 = 北京 2026-01-01 11:00：与 ② 同桶，接着顺延而非重新从 1 开始。
	laterSameBeijingDay := time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		now        time.Time
		wantNumber string
		wantYear   int
	}{
		{name: "北京 2025-12-31 23:30 取号", now: lastMomentOfBeijing2025, wantNumber: "GTSR-250001", wantYear: 2025},
		{name: "北京 2026-01-01 00:30（UTC 仍是 2025-12-31）取号", now: firstMomentOfBeijing2026, wantNumber: "GTSR-260001", wantYear: 2026},
		{name: "北京 2026-01-01 11:00 取号（与上一条同桶）", now: laterSameBeijingDay, wantNumber: "GTSR-260002", wantYear: 2026},
	}
	for _, item := range cases {
		number, err := allocateInIndependentTx(r, tenant, "", item.now)
		if err != nil {
			t.Fatalf("%s: 取号失败: %v", item.name, err)
		}
		if number != item.wantNumber {
			t.Fatalf("%s: 单号=%q，want %q（年份段必须按北京日历日）", item.name, number, item.wantNumber)
		}
		if got := businessclock.YearOf(item.now); got != item.wantYear {
			t.Fatalf("%s: 业务年份=%d，want %d", item.name, got, item.wantYear)
		}
		rows := registryRowsFor(t, r, number)
		if len(rows) != 1 {
			t.Fatalf("%s: 登记表行数=%d，want 1", item.name, len(rows))
		}
		if int(rows[0].SeqYear) != item.wantYear {
			t.Fatalf("%s: 登记行 seq_year=%d，want %d", item.name, rows[0].SeqYear, item.wantYear)
		}
		if !rows[0].IssuedAt.UTC().Equal(item.now.UTC()) {
			t.Fatalf("%s: 登记行 issued_at=%v，want 原样 UTC 时刻 %v（存储时间戳不改成北京时间）",
				item.name, rows[0].IssuedAt.UTC(), item.now.UTC())
		}
	}

	// 流水桶只应出现北京日历年的两个年度：2025 一个桶、2026 一个桶，
	// 且 2026 桶的水位是 2（① 不落在 2026，② ③ 落在同一 2026 桶）。
	var sequenceRows []contractNumberSequenceRecord
	if err := r.db.Where("rule_key = ?", key).Order("seq_year ASC").Find(&sequenceRows).Error; err != nil {
		t.Fatalf("读取年度流水桶: %v", err)
	}
	if len(sequenceRows) != 2 {
		t.Fatalf("流水桶年度=%+v，want 恰好 2025 与 2026 两个桶", sequenceRows)
	}
	if sequenceRows[0].SeqYear != 2025 || sequenceRows[1].SeqYear != 2026 {
		t.Fatalf("流水桶年度=%d/%d，want 2025/2026", sequenceRows[0].SeqYear, sequenceRows[1].SeqYear)
	}
	if sequenceRows[0].CurrentValue != 1 || sequenceRows[1].CurrentValue != 2 {
		t.Fatalf("流水桶水位=%d/%d，want 1/2", sequenceRows[0].CurrentValue, sequenceRows[1].CurrentValue)
	}
	for _, year := range []int{2025, 2026} {
		if current, err := r.ContractNumberSequenceValue(ctx, key, year); err != nil || current == 0 {
			t.Fatalf("%d 年水位=%d err=%v，want 非零", year, current, err)
		}
	}
}

// TestContractNumberRuleChangeRestartsCurrentYearOnIsolatedMySQL 验证改规则后当年流水从 1 重新开始，
// 且把规则改回原样时回到原来的桶继续递增（不重号）。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberRuleChangeRestartsCurrentYearOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	first := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	second := contract.ContractNumberRule{Prefix: "ABCD-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, first, second)
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)

	saveContractNumberRule(t, r, first)
	for index := 1; index <= 3; index++ {
		number, err := allocateInIndependentTx(r, tenant, "", now)
		if err != nil {
			t.Fatalf("原规则取号 %d: %v", index, err)
		}
		if want := fmt.Sprintf("GTSR-2600%02d", index); number != want {
			t.Fatalf("原规则取号 %d got %q want %q", index, number, want)
		}
	}

	// 改规则即换桶：当年流水从 1 重新开始，不会接着旧水位跳号。
	saveContractNumberRule(t, r, second)
	if number, err := allocateInIndependentTx(r, tenant, "", now); err != nil || number != "ABCD-260001" {
		t.Fatalf("改规则后首次取号=%q err=%v want ABCD-260001", number, err)
	}
	if number, err := allocateInIndependentTx(r, tenant, "", now); err != nil || number != "ABCD-260002" {
		t.Fatalf("改规则后第二次取号=%q err=%v want ABCD-260002", number, err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, contractNumberRuleKey(t, first), 2026); err != nil || current != 3 {
		t.Fatalf("原规则桶水位=%d err=%v want 3（改规则不得动原桶）", current, err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, contractNumberRuleKey(t, second), 2026); err != nil || current != 2 {
		t.Fatalf("新规则桶水位=%d err=%v want 2", current, err)
	}

	// 改回原规则：回到原来的桶继续，不重号也不从 1 重来。
	saveContractNumberRule(t, r, first)
	if number, err := allocateInIndependentTx(r, tenant, "", now); err != nil || number != "GTSR-260004" {
		t.Fatalf("改回原规则后取号=%q err=%v want GTSR-260004", number, err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, contractNumberRuleKey(t, first), 2026); err != nil || current != 4 {
		t.Fatalf("改回后原规则桶水位=%d err=%v want 4", current, err)
	}
}

// TestContractNumberSequenceAllocationOnIsolatedMySQL 验证年度隔离与全系统并发唯一。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberSequenceAllocationOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	ctx := context.Background()
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	key := contractNumberRuleKey(t, rule)
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)

	for index := 1; index <= 3; index++ {
		number, err := allocateInIndependentTx(r, tenant, "HT-{YYYYMMDD}-{ID8}", now)
		if err != nil {
			t.Fatalf("allocate %d: %v", index, err)
		}
		want := fmt.Sprintf("GTSR-2600%02d", index)
		if number != want {
			t.Fatalf("allocate %d got %q want %q", index, number, want)
		}
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2026); err != nil || current != 3 {
		t.Fatalf("sequence=%d err=%v want 3", current, err)
	}

	// 年度隔离：次年从 1 重新开始，且不影响本年水位。
	nextYear, err := allocateInIndependentTx(r, tenant, "", now.AddDate(1, 0, 0))
	if err != nil || nextYear != "GTSR-270001" {
		t.Fatalf("next year number=%q err=%v", nextYear, err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2027); err != nil || current != 1 {
		t.Fatalf("sequence 2027=%d err=%v want 1", current, err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2026); err != nil || current != 3 {
		t.Fatalf("sequence 2026=%d err=%v want 3", current, err)
	}

	// 并发取号：所有并发全部落在同一个 (年度, 规则指纹) 桶上，且每个并发使用不同租户，
	// 因此唯一性不能靠租户内唯一键兜底，只能由全局流水与登记表主键保证。
	const workers = 12
	numbers := make([]string, workers)
	errs := make([]error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			numbers[slot], errs[slot] = allocateInIndependentTx(r, ulid.Make().String(), "", now)
		}(index)
	}
	group.Wait()
	seen := make(map[string]bool, workers)
	for index := 0; index < workers; index++ {
		if errs[index] != nil {
			t.Fatalf("concurrent allocate %d: %v", index, errs[index])
		}
		if seen[numbers[index]] {
			t.Fatalf("并发取号重复: %s", numbers[index])
		}
		seen[numbers[index]] = true
	}
	// 桶内严格递增，因此并发结果必须恰好覆盖 0004 到 0015，既不重复也不跳号。
	for sequence := 4; sequence < 4+workers; sequence++ {
		want := fmt.Sprintf("GTSR-26%04d", sequence)
		if !seen[want] {
			t.Fatalf("并发取号缺少 %s，实际=%v", want, numbers)
		}
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2026); err != nil || current != uint64(3+workers) {
		t.Fatalf("concurrent sequence=%d err=%v want %d", current, err, 3+workers)
	}
}

// TestContractNumberAllocationSkipsTakenNumberOnIsolatedMySQL 验证编号被登记表占用时顺延。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberAllocationSkipsTakenNumberOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	taken, err := contract.BuildContractNumber(rule, 2026, 1)
	if err != nil {
		t.Fatal(err)
	}
	seedContractNumberHolder(t, r, tenant, taken)
	number, err := allocateInIndependentTx(r, tenant, "", now)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if number != "GTSR-260002" {
		t.Fatalf("got %q want GTSR-260002", number)
	}
}

// TestContractNumberAllocationExhaustionOnIsolatedMySQL 验证位宽耗尽返回业务错误。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberAllocationExhaustionOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	ctx := context.Background()
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 1}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	key := contractNumberRuleKey(t, rule)
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	for index := 1; index <= 9; index++ {
		if _, err := allocateInIndependentTx(r, tenant, "", now); err != nil {
			t.Fatalf("allocate %d: %v", index, err)
		}
	}
	if _, err := allocateInIndependentTx(r, tenant, "", now); !errors.Is(err, contract.ErrNumberExhausted) {
		t.Fatalf("err=%v want ErrNumberExhausted", err)
	}
	// 失败必须整体回滚，水位停在上限值，重试仍稳定失败。
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2026); err != nil || current != 9 {
		t.Fatalf("sequence=%d err=%v want 9", current, err)
	}
	if _, err := allocateInIndependentTx(r, tenant, "", now); !errors.Is(err, contract.ErrNumberExhausted) {
		t.Fatalf("retry err=%v want ErrNumberExhausted", err)
	}
}

// TestContractNumberAllocationSkipsHistoricalNumberAcrossTenantsOnIsolatedMySQL 覆盖缺陷 1：
// 规则字符串空间与历史重叠时的跨租户重号。
//
// 构造：租户 B 历史上用「GTSR-」+ 不含年份段 + 4 位流水签发了 GTSR-260001（等价于
// "GTSR-"+"260001"），登记表里该号归租户 B。现在租户 A 在「GTSR-」+YY+4 位的新桶上
// 取号，流水从 1 开始，候选号字符串恰好又是 GTSR-260001。取号必须看到全系统登记表，
// 得到 GTSR-260002；只查本租户的旧实现会把它再签发一次。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberAllocationSkipsHistoricalNumberAcrossTenantsOnIsolatedMySQL(t *testing.T) {
	r, tenantA := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	tenantB := ulid.Make().String()

	// 历史编号不带年份段：直接按字符串占住 GTSR-260001，登记表行没有规则桶信息。
	seedContractNumberHolder(t, r, tenantB, "GTSR-260001")

	number, err := allocateInIndependentTx(r, tenantA, "", now)
	if err != nil {
		t.Fatalf("租户 A 取号: %v", err)
	}
	if number != "GTSR-260002" {
		t.Fatalf("租户 A 取号=%q want GTSR-260002（不得复用租户 B 的历史编号）", number)
	}

	// 登记表归属必须正确：260001 属 B，260002 属 A，且各只有一行。
	type registryRow struct {
		ContractNumber string
		TenantID       string
	}
	var rows []registryRow
	if err := r.db.Model(&contractNumberRegistryRecord{}).
		Where("contract_number IN ?", []string{"GTSR-260001", "GTSR-260002"}).
		Order("contract_number ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read registry: %v", err)
	}
	if len(rows) != 2 || rows[0].ContractNumber != "GTSR-260001" || rows[0].TenantID != tenantB ||
		rows[1].ContractNumber != "GTSR-260002" || rows[1].TenantID != tenantA {
		t.Fatalf("registry rows=%+v want 260001→tenantB, 260002→tenantA", rows)
	}

	// 连续再取三个号：历史号只有 1 个，仍在 50 次顺延预算内，且不与历史号重复。
	seen := map[string]bool{"GTSR-260001": true, "GTSR-260002": true}
	for index := 0; index < 3; index++ {
		next, err := allocateInIndependentTx(r, ulid.Make().String(), "", now)
		if err != nil {
			t.Fatalf("后续取号 %d: %v", index, err)
		}
		if seen[next] {
			t.Fatalf("跨租户重号: %s", next)
		}
		seen[next] = true
	}
}

// TestContractNumberSequenceLossDoesNotReissueNumbersOnIsolatedMySQL 覆盖缺陷 2：
// con_contract_number_sequence 被清空（truncate 或从缺流水行的备份恢复）后，
// 同一规则桶从 1 重新开始，不能把已签发过的编号再发一次。
// 参数：t 为测试上下文；仅显式 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberSequenceLossDoesNotReissueNumbersOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	ctx := context.Background()
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	key := contractNumberRuleKey(t, rule)
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)

	first, err := allocateInIndependentTx(r, tenant, "", now)
	if err != nil || first != "GTSR-260001" {
		t.Fatalf("首次取号=%q err=%v want GTSR-260001", first, err)
	}

	// 模拟流水行丢失：桶回到零水位。
	if err := r.db.Where("seq_year = ? AND rule_key = ?", 2026, key).Delete(&contractNumberSequenceRecord{}).Error; err != nil {
		t.Fatalf("drop sequence row: %v", err)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, 2026); err != nil || current != 0 {
		t.Fatalf("清空后水位=%d err=%v want 0", current, err)
	}

	// 流水又从 1 开始，但登记表已占 260001，因此必须顺延。
	second, err := allocateInIndependentTx(r, tenant, "", now)
	if err != nil {
		t.Fatalf("流水丢失后取号: %v", err)
	}
	if second != "GTSR-260002" {
		t.Fatalf("流水丢失后取号=%q want GTSR-260002（不得重复签发 260001）", second)
	}

	// 再丢一次流水：已签发 260001、260002 都在登记表里，只能继续顺延到 260003。
	if err := r.db.Where("seq_year = ? AND rule_key = ?", 2026, key).Delete(&contractNumberSequenceRecord{}).Error; err != nil {
		t.Fatalf("drop sequence row again: %v", err)
	}
	third, err := allocateInIndependentTx(r, tenant, "", now)
	if err != nil {
		t.Fatalf("二次流水丢失后取号: %v", err)
	}
	if third != "GTSR-260003" {
		t.Fatalf("二次流水丢失后取号=%q want GTSR-260003", third)
	}

	// 登记表恰好记录这三个号，没有重复行，也没有把别人的号记到本租户之外。
	var total int64
	if err := r.db.Model(&contractNumberRegistryRecord{}).Where("contract_number LIKE ?", "GTSR-26%").Count(&total).Error; err != nil {
		t.Fatalf("count registry: %v", err)
	}
	if total != 3 {
		t.Fatalf("登记表行数=%d want 3", total)
	}
	var tenants []string
	if err := r.db.Model(&contractNumberRegistryRecord{}).Where("contract_number LIKE ?", "GTSR-26%").
		Order("contract_number ASC").Pluck("tenant_id", &tenants).Error; err != nil {
		t.Fatalf("read registry tenants: %v", err)
	}
	for _, owner := range tenants {
		if owner != tenant {
			t.Fatalf("登记表归属=%v want 全部 %s", tenants, tenant)
		}
	}
}

// contractNumberE2ERepositoryWithTranslateError 用与生产完全一致的 GORM 配置
// （PrepareStmt + TranslateError）构造仓储。
//
// 共享 helper draftE2ERepository 没开 TranslateError，只能走到裸 *mysql.MySQLError 分支；
// 而生产 internal/bootstrap/dependencies.go 打开了它，1062 会被翻译成 gorm.ErrDuplicatedKey。
// 为了不改变既有 E2E 的连接配置（避免影响同包其它用例），这里单独建连，
// 并复用同一套隔离库校验与按租户清理逻辑。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：与生产配置一致的仓储和随机测试租户。
func contractNumberE2ERepositoryWithTranslateError(t *testing.T) (*Repository, string) {
	t.Helper()
	dsn := draftE2ETestDSN(t)
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		PrepareStmt:    true,
		TranslateError: true,
	})
	if err != nil {
		t.Fatal("dedicated test database connection failed")
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(8)
	tenant := ulid.Make().String()
	t.Cleanup(func() {
		draftE2ECleanupTenant(t, db, sqlDB, tenant)
		if err := sqlDB.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewRepository(db), tenant
}

// startContractNumberApproval 启动一次真实合同审批，返回活动入参供 CompleteApproval 使用。
//
// 参数：t 为测试上下文，r 为仓储，tenant 为租户，item 为草稿合同。
// 返回值：审批 ID 与完成审批的活动入参。
func startContractNumberApproval(t *testing.T, r *Repository, tenant string, item contract.Contract, workflowID string) (string, workflows.CompleteApprovalActivityInput) {
	t.Helper()
	approvalID := ulid.Make().String()
	if err := r.StartApproval(context.Background(), workflows.StartApprovalActivityInput{
		ApprovalID: approvalID, TenantID: tenant, ContractID: item.ID, ExpectedVersion: 1,
		Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending,
		ContentHash: item.ContentHash, ApplicantUserID: item.OwnerUserID, WorkflowID: workflowID, RunID: "run-" + workflowID,
		Nodes: []approval.Node{{ID: "node-1", Name: "销售审批", RoleCode: "sales_director",
			AssigneeIDs: []string{"approver-1"}, Countersign: approval.CountersignAny}},
	}); err != nil {
		t.Fatalf("StartApproval: %v", err)
	}
	return approvalID, workflows.CompleteApprovalActivityInput{
		ApprovalID: approvalID, TenantID: tenant, ContractID: item.ID, ActorUserID: "approver-1",
		Status: approval.StatusApproved, WorkflowID: workflowID,
	}
}

// registryRowsFor 读取某个编号在登记表里的全部行，用于断言“恰好一行且归属正确”。
//
// 参数：t 为测试上下文，r 为仓储，number 为合同编号。
// 返回值：登记表行；查询失败直接 fail。
func registryRowsFor(t *testing.T, r *Repository, number string) []contractNumberRegistryRecord {
	t.Helper()
	var rows []contractNumberRegistryRecord
	if err := r.db.Where("contract_number = ?", number).Find(&rows).Error; err != nil {
		t.Fatalf("read registry for %s: %v", number, err)
	}
	return rows
}

// TestContractNumberAllocationSkipsTakenNumberWithTranslateErrorOnIsolatedMySQL 覆盖生产实际的
// 1062 分支：bootstrap 打开了 GORM TranslateError，唯一键冲突被翻译成 gorm.ErrDuplicatedKey，
// 而共享 E2E helper 没开该开关，历史上这条分支零覆盖。
//
// 断言：①当前配置下重复写入确实被翻译成 gorm.ErrDuplicatedKey（否则用例会退化成重复
// 覆盖驱动分支，必须直接失败）；②撞号后仍能顺延到下一个号；③登记表恰好两行且归属正确。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberAllocationSkipsTakenNumberWithTranslateErrorOnIsolatedMySQL(t *testing.T) {
	r, tenant := contractNumberE2ERepositoryWithTranslateError(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	now := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	taken, err := contract.BuildContractNumber(rule, 2026, 1)
	if err != nil {
		t.Fatal(err)
	}
	seedContractNumberHolder(t, r, tenant, taken)

	// 探针：确认本用例确实走在 gorm.ErrDuplicatedKey 分支上（生产配置），而不是裸驱动错误。
	probe := r.db.Exec(`INSERT INTO con_contract_number_registry (contract_number, tenant_id, contract_id)
		VALUES (?, 'probe-tenant', 'probe-contract')`, taken).Error
	if !errors.Is(probe, gorm.ErrDuplicatedKey) {
		t.Fatalf("TranslateError 配置下 1062 必须被翻译成 gorm.ErrDuplicatedKey，实际 err=%v", probe)
	}
	if isContractNumberDuplicate(probe) != true {
		t.Fatalf("isContractNumberDuplicate 必须识别翻译后的重复键错误: %v", probe)
	}

	number, err := allocateInIndependentTx(r, tenant, "", now)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if number != "GTSR-260002" {
		t.Fatalf("got %q want GTSR-260002（翻译后的重复键也必须顺延）", number)
	}
	if rows := registryRowsFor(t, r, "GTSR-260002"); len(rows) != 1 || rows[0].TenantID != tenant {
		t.Fatalf("registry rows=%+v want 恰好一行且属本租户", rows)
	}
	if rows := registryRowsFor(t, r, "GTSR-260001"); len(rows) != 1 || rows[0].TenantID != tenant {
		t.Fatalf("历史号登记行被改动: %+v", rows)
	}
}

// TestCompleteApprovalAppliesRuleNumberOnceOnMySQL 用真实 StartApproval + CompleteApproval
// 端到端验证审批取号闭环：
//  1. con_contract.contract_number 等于按系统级规则拼出的串（GTSR- + YY + 4 位）；
//  2. 登记表恰好一行，tenant/contract 与合同行一致；
//  3. 流水水位精确等于 1（只消耗一次）；
//  4. 完成后再重复调用（含并发）编号不变、水位仍为 1、登记表仍一行。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestCompleteApprovalAppliesRuleNumberOnceOnMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	t.Cleanup(func() { resetContractNumberSequence(t, r, rule) })
	key := contractNumberRuleKey(t, rule)
	ctx := context.Background()
	// 生产取号的年份段按北京日历日取年，断言必须同口径，否则北京时间 1 月 1 日 00:00–08:00 会误判。
	year := businessclock.YearOf(time.Now())
	want := fmt.Sprintf("GTSR-%02d0001", year%100)

	item := stampedE2EContract(t, r, tenant, contract.StatusDraft)
	_, input := startContractNumberApproval(t, r, tenant, item, "wf-number-once")
	if err := r.CompleteApproval(ctx, input); err != nil {
		t.Fatalf("CompleteApproval: %v", err)
	}

	// ① 合同行编号等于按规则拼出的串。
	stored, err := r.GetContract(ctx, tenant, item.ID)
	if err != nil {
		t.Fatalf("GetContract: %v", err)
	}
	if stored.Number != want {
		t.Fatalf("contract_number=%q want %q", stored.Number, want)
	}
	// ② 登记表恰好一行，归属与合同行一致。
	rows := registryRowsFor(t, r, want)
	if len(rows) != 1 {
		t.Fatalf("登记表行数=%d want 1: %+v", len(rows), rows)
	}
	if rows[0].TenantID != tenant || rows[0].ContractID != item.ID {
		t.Fatalf("登记表归属=%+v want tenant=%s contract=%s", rows[0], tenant, item.ID)
	}
	// ③ 流水只消耗一次。
	if current, err := r.ContractNumberSequenceValue(ctx, key, year); err != nil || current != 1 {
		t.Fatalf("sequence=%d err=%v want 1（取号只消耗一次）", current, err)
	}

	// ④ 重复与并发重试不得再取号。
	if err := r.CompleteApproval(ctx, input); err != nil {
		t.Fatalf("repeat CompleteApproval: %v", err)
	}
	var group sync.WaitGroup
	errs := make([]error, 2)
	for index := 0; index < 2; index++ {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			errs[slot] = r.CompleteApproval(ctx, input)
		}(index)
	}
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent retry %d: %v", index, err)
		}
	}
	repeated, err := r.GetContract(ctx, tenant, item.ID)
	if err != nil {
		t.Fatalf("re-read contract: %v", err)
	}
	if repeated.Number != want {
		t.Fatalf("重试后 contract_number=%q want 不变 %q", repeated.Number, want)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, year); err != nil || current != 1 {
		t.Fatalf("重试后 sequence=%d err=%v want 仍为 1（不得烧号）", current, err)
	}
	if rows := registryRowsFor(t, r, want); len(rows) != 1 {
		t.Fatalf("重试后登记表行数=%d want 1", len(rows))
	}
	var total int64
	if err := r.db.Model(&contractNumberRegistryRecord{}).Where("contract_number LIKE ?", "GTSR-%").Count(&total).Error; err != nil {
		t.Fatalf("count registry: %v", err)
	}
	if total != 1 {
		t.Fatalf("本用例只应登记一个 GTSR- 编号，实际 %d", total)
	}
}

// TestCompleteApprovalIssuesNumberForEmptyStringNumberOnMySQL 覆盖“存量空串编号”场景：
// 000038 回填条件显式排除 ”，说明历史库可能把“无编号”存成空串。审批写号谓词必须把
// NULL 与 ” 同等视为待发号，并在影响行数不是 1 时显式失败，不能静默提交。
//
// 断言：审批后合同行拿到按规则拼出的编号，登记表恰好一行，水位为 1。
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestCompleteApprovalIssuesNumberForEmptyStringNumberOnMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	t.Cleanup(func() { resetContractNumberSequence(t, r, rule) })
	key := contractNumberRuleKey(t, rule)
	ctx := context.Background()
	// 生产取号的年份段按北京日历日取年，断言必须同口径，否则北京时间 1 月 1 日 00:00–08:00 会误判。
	year := businessclock.YearOf(time.Now())
	want := fmt.Sprintf("GTSR-%02d0001", year%100)

	item := stampedE2EContract(t, r, tenant, contract.StatusDraft)
	// 模拟历史库：编号列不是 NULL 而是空串。
	if err := r.db.Exec(`UPDATE con_contract SET contract_number = '' WHERE tenant_id = ? AND id = ?`,
		tenant, item.ID).Error; err != nil {
		t.Fatalf("seed empty contract_number: %v", err)
	}
	_, input := startContractNumberApproval(t, r, tenant, item, "wf-number-empty")
	if err := r.CompleteApproval(ctx, input); err != nil {
		t.Fatalf("CompleteApproval: %v", err)
	}
	stored, err := r.GetContract(ctx, tenant, item.ID)
	if err != nil {
		t.Fatalf("GetContract: %v", err)
	}
	if stored.Number != want {
		t.Fatalf("空串编号未被补号: contract_number=%q want %q", stored.Number, want)
	}
	if rows := registryRowsFor(t, r, want); len(rows) != 1 || rows[0].ContractID != item.ID {
		t.Fatalf("登记表行=%+v want 恰好一行且属本合同", rows)
	}
	if current, err := r.ContractNumberSequenceValue(ctx, key, year); err != nil || current != 1 {
		t.Fatalf("sequence=%d err=%v want 1", current, err)
	}
}

// seedSameTenantNumberHolder 在同一租户下写入一份已持有编号的合同行，且不写登记表。
//
// 用于构造“登记表与合同行失步”的事故现场：取号会认为该号可用，写号时才撞上
// uk_con_contract_number(tenant_id, contract_number)。清理完全交给
// draftE2ERepository 的按租户清理（契约：本 helper 只用于同一个测试租户）。
//
// 参数：t 为测试上下文，r 为仓储，tenantID 为租户，number 为该租户已占用的编号。
// 返回值：无。
func seedSameTenantNumberHolder(t *testing.T, r *Repository, tenantID, number string) {
	t.Helper()
	now := time.Now().UTC()
	record := contractRecord{
		ID: ulid.Make().String(), TenantID: tenantID, ContractNumber: &number, Title: "同租户历史编号",
		ContractType: "sales", ServiceType: "integration", OwnerUserID: ulid.Make().String(),
		AmountMinor: 100, Currency: "CNY", Content: "{}", Status: string(contract.StatusActive),
		Version: 1, CreatedAt: now, CreatedBy: "seed", UpdatedAt: now, UpdatedBy: "seed",
	}
	if err := r.db.Create(&record).Error; err != nil {
		t.Fatalf("seed same-tenant number holder: %v", err)
	}
}

// TestCompleteApprovalReportsNumberConflictOnIsolatedMySQL 覆盖 P1-B：
// 审批取号后写号命中 uk_con_contract_number(tenant_id, contract_number) 时必须返回
// 可识别的业务错误（contract.ErrNumberConflict，供工作流判为 NonRetryable），
// 而不是把 1062 当执行故障无限重试，也不得烧掉流水或留下登记表行。
//
// 构造方式贴近真实事故：登记表与合同行失步——本租户已有一份合同持有 GTSR-260001，
// 但登记表里没有该号（例如历史直写库或登记表被清理过）。取号会认为 260001 可用，
// 写号时才撞上租户内唯一键。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestCompleteApprovalReportsNumberConflictOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	restoreDefaultContractNumberRule(t, r)
	rule := contract.ContractNumberRule{Prefix: "GTSR-", YearSegment: contract.NumberYearSegmentYY, SeqWidth: 4}
	resetContractNumberState(t, r, rule)
	saveContractNumberRule(t, r, rule)
	t.Cleanup(func() { resetContractNumberSequence(t, r, rule) })
	ctx := context.Background()
	// 生产取号的年份段按北京日历日取年，断言必须同口径，否则北京时间 1 月 1 日 00:00–08:00 会误判。
	year := businessclock.YearOf(time.Now())
	conflicting := fmt.Sprintf("GTSR-%02d0001", year%100)

	// 本租户已有一份持有该号的合同，但登记表里没有这个号（登记表与合同行失步）。
	seedSameTenantNumberHolder(t, r, tenant, conflicting)

	item := stampedE2EContract(t, r, tenant, contract.StatusDraft)
	_, input := startContractNumberApproval(t, r, tenant, item, "wf-number-conflict")
	err := r.CompleteApproval(ctx, input)
	if !errors.Is(err, contract.ErrNumberConflict) {
		t.Fatalf("err=%v want contract.ErrNumberConflict", err)
	}
	if !strings.Contains(err.Error(), "contract-number-audit") {
		t.Fatalf("错误必须给出可执行指引: %v", err)
	}
	// 事务整体回滚：合同仍未发号、状态仍为审批中，登记表没有该号，流水未消耗。
	stored, readErr := r.GetContract(ctx, tenant, item.ID)
	if readErr != nil {
		t.Fatalf("GetContract: %v", readErr)
	}
	if stored.Number != "" || stored.Status != contract.StatusPending {
		t.Fatalf("失败后合同状态=%s number=%q，应当整体回滚", stored.Status, stored.Number)
	}
	if rows := registryRowsFor(t, r, conflicting); len(rows) != 0 {
		t.Fatalf("写号失败不得留下登记行: %+v", rows)
	}
	if current, seqErr := r.ContractNumberSequenceValue(ctx, contractNumberRuleKey(t, rule), year); seqErr != nil || current != 0 {
		t.Fatalf("写号失败不得烧号: sequence=%d err=%v want 0", current, seqErr)
	}
}

// 编译期断言：生产配置的仓储确实实现了保存规则前的登记表干跑能力。
var _ interface {
	ContractNumberRegistryContains(context.Context, string) (bool, error)
} = (*Repository)(nil)

// TestIsContractNumberRelationMissing 固定“只有表不存在才允许降级”的判定口径。
//
// 这个判定是取号降级的唯一入口：误判会把任何读错误都当成“未迁移”，
// 从而绕过全系统登记表、让唯一性闭环出现缺口。因此这里覆盖驱动码、被 %w 包装的
// 驱动码、纯文本兜底，以及必须返回 false 的其它错误。
//
// 参数：t 为测试上下文。
// 返回值：无。
func TestIsContractNumberRelationMissing(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "表不存在1146", err: &driver.MySQLError{Number: 1146, Message: "Table 'x' doesn't exist"}, want: true},
		{name: "包装后的1146", err: fmt.Errorf("read rule: %w", &driver.MySQLError{Number: 1146}), want: true},
		{name: "文本兜底Error1146", err: errors.New("Error 1146: Table 'con_contract_number_rule' doesn't exist"), want: true},
		{name: "唯一键冲突不是缺表", err: &driver.MySQLError{Number: 1062, Message: "Duplicate entry"}, want: false},
		{name: "翻译后的重复键不是缺表", err: gorm.ErrDuplicatedKey, want: false},
		{name: "连接已关闭不是缺表", err: sql.ErrConnDone, want: false},
		{name: "记录不存在不是缺表", err: gorm.ErrRecordNotFound, want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, testCase := range cases {
		if got := isContractNumberRelationMissing(testCase.err); got != testCase.want {
			t.Fatalf("%s: got %v want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestIsContractNumberDuplicate 固定两种重复键识别方式：驱动码与 GORM 翻译结果。
// 参数：t 为测试上下文。
// 返回值：无。
func TestIsContractNumberDuplicate(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "驱动码1062", err: &driver.MySQLError{Number: 1062}, want: true},
		{name: "翻译后的重复键", err: gorm.ErrDuplicatedKey, want: true},
		{name: "包装后的重复键", err: fmt.Errorf("insert: %w", gorm.ErrDuplicatedKey), want: true},
		{name: "缺表不是重复键", err: &driver.MySQLError{Number: 1146}, want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, testCase := range cases {
		if got := isContractNumberDuplicate(testCase.err); got != testCase.want {
			t.Fatalf("%s: got %v want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestContractNumberRegisteredInTxUsesLockingReadOnIsolatedMySQL 固定 1062 后的确认查询必须是
// 加锁读：事务是 REPEATABLE READ，并发事务在本事务快照建立之后提交的登记行，普通快照读
// 看不到——那会把真实的“号被占”误判成别的唯一键故障，让取号直接失败而不是顺延。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberRegisteredInTxUsesLockingReadOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	number := "GTSR-LOCK-" + strings.ToUpper(ulid.Make().String()[:8])

	tx := r.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatalf("begin: %v", tx.Error)
	}
	defer func() {
		if err := tx.Rollback().Error; err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback: %v", err)
		}
	}()
	// 先在事务里做一次普通读，建立快照。
	var marker int
	if err := tx.Raw(`SELECT 1 FROM con_contract_number_rule WHERE id = 1`).Row().Scan(&marker); err != nil {
		t.Fatalf("establish snapshot: %v", err)
	}
	// 另一条连接提交登记行（模拟并发审批抢先占号）。
	if err := r.db.Exec(`INSERT INTO con_contract_number_registry (contract_number, tenant_id, contract_id)
		VALUES (?, ?, ?)`, number, tenant, ulid.Make().String()).Error; err != nil {
		t.Fatalf("seed registry row: %v", err)
	}
	// 快照读看不到新提交的行……
	var snapshot int
	snapshotErr := tx.Raw(`SELECT 1 FROM con_contract_number_registry WHERE contract_number = ? LIMIT 1`, number).Row().Scan(&snapshot)
	if !errors.Is(snapshotErr, sql.ErrNoRows) {
		t.Fatalf("快照读本应看不到并发提交的行，实际 err=%v", snapshotErr)
	}
	// ……加锁读必须看到，据此才能继续顺延而不是报错。
	registered, err := contractNumberRegisteredInTx(tx, number)
	if err != nil {
		t.Fatalf("contractNumberRegisteredInTx: %v", err)
	}
	if !registered {
		t.Fatal("加锁读必须看到并发提交的登记行，否则真实撞号会被误判成其它唯一键故障")
	}
	// 未登记的号必须返回 false。
	if registered, err := contractNumberRegisteredInTx(tx, number+"-absent"); err != nil || registered {
		t.Fatalf("未登记号 registered=%v err=%v want false/nil", registered, err)
	}
}

// TestContractNumberRegistryContainsOnIsolatedMySQL 验证保存规则前干跑用的只读查询：
// 登记表主键排序规则大小写不敏感，因此只改前缀大小写会命中历史号。
//
// 参数：t 为测试上下文；仅 CONTRACT_TEST_DSN 指向隔离库时运行。
// 返回值：无。
func TestContractNumberRegistryContainsOnIsolatedMySQL(t *testing.T) {
	r, tenant := draftE2ERepository(t)
	ctx := context.Background()
	number := "GTSR-CASE-" + strings.ToUpper(ulid.Make().String()[:8])
	if err := r.db.Exec(`INSERT INTO con_contract_number_registry (contract_number, tenant_id, contract_id)
		VALUES (?, ?, ?)`, number, tenant, ulid.Make().String()).Error; err != nil {
		t.Fatalf("seed registry row: %v", err)
	}
	occupied, err := r.ContractNumberRegistryContains(ctx, strings.ToLower(number))
	if err != nil {
		t.Fatalf("ContractNumberRegistryContains: %v", err)
	}
	if !occupied {
		t.Fatalf("小写写法 %s 必须命中已登记的大写编号 %s", strings.ToLower(number), number)
	}
	free, err := r.ContractNumberRegistryContains(ctx, number+"-FREE")
	if err != nil {
		t.Fatalf("ContractNumberRegistryContains: %v", err)
	}
	if free {
		t.Fatal("未登记的编号不得被判为已占用")
	}
}
