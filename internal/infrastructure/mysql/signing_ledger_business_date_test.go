package mysql

import (
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// 本用例不需要 CONTRACT_TEST_DSN：DryRun 只生成 SQL、不建立连接。
func newSigningLedgerDryRun(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		DSN:                       "test:test@tcp(localhost:3306)/test?parseTime=true",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, DryRun: true, SkipDefaultTransaction: true, Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open dry-run database: %v", err)
	}
	return db
}

// TestSigningLedgerDateParamsUseBeijingDateUTCWallClock 固定签署台账业务日期参数的落库口径。
//
// 这是最容易出错的一条：驱动 DSN 默认 loc=UTC，会把 time.Time 先转成 UTC 再格式化成
// 墙钟参数。因此“北京今天”必须以「同一天 00:00 UTC 墙钟」（CalendarDay）传入；若直接把
// 北京市区的 00:00（businessclock.Today()）传进去，参数会变成前一天 16:00，整条
// end_date 比较错开 8 小时。这里用 DryRun 断言实际绑定给 SQL 的时间参数。
func TestSigningLedgerDateParamsUseBeijingDateUTCWallClock(t *testing.T) {
	db := newSigningLedgerDryRun(t)
	today := businessclock.CalendarDay(businessclock.Now())
	beijingToday := businessclock.Today()

	query := applySigningRecordFilters(
		signingLedgerQuery(db, contract.ScopeFilter{TenantID: "tenant-1", AllowAll: true}),
		contract.SigningRecordQuery{Status: "expired"}, today)

	var records []contractRecord
	statement := query.Session(&gorm.Session{DryRun: true}).Find(&records).Statement
	if statement.SQL.String() == "" {
		t.Fatal("未捕获到签署台账查询 SQL")
	}
	if !strings.Contains(statement.SQL.String(), "c.end_date") {
		t.Fatalf("过期筛选必须落在 end_date 上：%s", statement.SQL.String())
	}
	var boundTimes []time.Time
	for _, value := range statement.Vars {
		if at, ok := value.(time.Time); ok {
			boundTimes = append(boundTimes, at)
		}
	}
	if len(boundTimes) == 0 {
		t.Fatal("签署台账筛选未绑定任何时间参数")
	}
	for _, bound := range boundTimes {
		// 驱动按 UTC 格式化墙钟：这里必须正好是北京当天 00:00:00，不能是 16:00:00。
		if wall := bound.UTC().Format("2006-01-02 15:04:05"); wall != beijingToday.Format(time.DateOnly)+" 00:00:00" {
			t.Fatalf("业务日期参数墙钟=%s，want %s 00:00:00（UTC 墙钟口径，不得偏移 8 小时）",
				wall, beijingToday.Format(time.DateOnly))
		}
	}
}

// TestSigningStatsMonthBoundUsesBeijingInstant 固定“本月已确认”的月界口径：
// confirmed_at 是绝对时刻的快照，月界必须是北京时间月初对应的真实瞬间（月界.UTC()），
// 而不是 today 的 UTC 0 点，否则北京时间每月 1 日 00:00–08:00 的确认会被漏统计。
func TestSigningStatsMonthBoundUsesBeijingInstant(t *testing.T) {
	source := readMySQLSource(t, "contracts_signing_query.go")
	start := strings.Index(source, "func (r *Repository) signingStatsScoped(")
	if start < 0 {
		t.Fatal("未找到 signingStatsScoped")
	}
	end := strings.Index(source[start:], "\n}\n")
	if end < 0 {
		t.Fatal("未找到 signingStatsScoped 结束位置")
	}
	body := source[start : start+end]
	if !strings.Contains(body, "monthStart := businessclock.MonthStart()") {
		t.Fatalf("月界必须取北京业务月：%s", body)
	}
	if !strings.Contains(body, "monthStart.UTC(), nextMonth.UTC()") {
		t.Fatalf("绝对时刻比较必须传北京时间月初对应的真实瞬间：%s", body)
	}
	if strings.Contains(body, "time.Date(today.Year()") {
		t.Fatalf("不得再用 today（UTC/数据库口径）反推绝对时刻月界：%s", body)
	}
	// 台账的“今天”只允许走 CalendarDay，禁止退回 UTC 的 time.Now。
	ledgerStart := strings.Index(source, "func (r *Repository) ListSigningRecordsScoped(")
	if ledgerStart < 0 {
		t.Fatal("未找到 ListSigningRecordsScoped")
	}
	ledgerEnd := strings.Index(source[ledgerStart:], "\n}\n")
	ledger := source[ledgerStart : ledgerStart+ledgerEnd]
	if !strings.Contains(ledger, "today := businessclock.CalendarDay(businessclock.Now())") {
		t.Fatalf("台账业务日期必须按北京日历日取：%s", ledger)
	}
	if strings.Contains(ledger, "businessclock.Today()") {
		t.Fatal("台账不得把北京市区 00:00 直接作为 SQL 参数（驱动换算后会偏移 8 小时）")
	}
}
