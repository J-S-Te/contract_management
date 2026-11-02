package application

import (
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestSigningRecordExpiredKeepsDueDayValid 固定内存兼容路径（signingRecordMatches /
// signingStatsOf）的「到期当天仍有效」口径，与 SQL 台账的 end_date < today 保持一致。
//
// 改前：内存路径用 EndDate.Before(time.Now().UTC())，到期日 00:00 一过就算过期，
// 与 SQL 路径差一天（到期当天在台账里显示“已失效”）。
// 改后：按北京业务日比较，到期当天仍有效，次日才算过期。
func TestSigningRecordExpiredKeepsDueDayValid(t *testing.T) {
	today := businessclock.Today()
	// PostgreSQL/MySQL 的 DATE 列经驱动读回是 UTC 0 点，这里按同一口径构造。
	dueToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	dueYesterday := dueToday.AddDate(0, 0, -1)
	expiredQuery := contract.SigningRecordQuery{Status: "expired"}

	recordDueToday := contract.SigningRecord{
		Status:   contract.SigningPendingShipment,
		Contract: contract.Contract{EndDate: &dueToday},
	}
	if signingRecordMatches(recordDueToday, expiredQuery) {
		t.Fatalf("到期当天（%s）仍应有效：内存路径不得把它筛选为“已失效”", dueToday.Format(time.DateOnly))
	}
	stats := signingStatsOf([]contract.SigningRecord{recordDueToday})
	if stats.Expired != 0 || stats.Processing != 1 || stats.Total != 1 {
		t.Fatalf("到期当天的统计=%+v，want 过期 0、处理中 1", stats)
	}

	recordDueYesterday := contract.SigningRecord{
		Status:   contract.SigningPendingShipment,
		Contract: contract.Contract{EndDate: &dueYesterday},
	}
	if !signingRecordMatches(recordDueYesterday, expiredQuery) {
		t.Fatalf("到期日为昨天（%s）必须判定为已失效", dueYesterday.Format(time.DateOnly))
	}
	stats = signingStatsOf([]contract.SigningRecord{recordDueYesterday})
	if stats.Expired != 1 || stats.Processing != 0 {
		t.Fatalf("到期日为昨天的统计=%+v，want 过期 1、处理中 0", stats)
	}

	// 到期日以北京时区 00:00 表示时也必须按同一日历日判断，不能被 8 小时偏移判成更早。
	beijingDueToday := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, businessclock.Location())
	recordBeijingDueToday := contract.SigningRecord{
		Status:   contract.SigningPendingShipment,
		Contract: contract.Contract{EndDate: &beijingDueToday},
	}
	if signingRecordMatches(recordBeijingDueToday, expiredQuery) {
		t.Fatal("同一业务日历日的北京时区表示不得被判定为已失效")
	}
}

// TestSigningStatsMonthlyUsesBeijingMonth 固定内存路径的“本月已确认”月界：
// ConfirmedAt 是绝对时刻，月界必须取北京时间本月 1 日 00:00 对应的真实瞬间。
func TestSigningStatsMonthlyUsesBeijingMonth(t *testing.T) {
	monthStart := businessclock.MonthStart()
	// 北京本月 1 日 00:00 正好等于 UTC 上个月最后一天 16:00：按 UTC 取月界会把这段时刻漏掉。
	justStarted := contract.SigningRecord{
		Status:      contract.SigningCompleted,
		ConfirmedAt: &monthStart,
		Contract:    contract.Contract{},
	}
	if stats := signingStatsOf([]contract.SigningRecord{justStarted}); stats.Monthly != 1 || stats.Completed != 1 {
		t.Fatalf("北京本月 1 日 00:00 确认的记录统计=%+v，want 本月已确认 1", stats)
	}
	beforeMonth := monthStart.Add(-time.Second)
	justBefore := contract.SigningRecord{
		Status:      contract.SigningCompleted,
		ConfirmedAt: &beforeMonth,
		Contract:    contract.Contract{},
	}
	if stats := signingStatsOf([]contract.SigningRecord{justBefore}); stats.Monthly != 0 || stats.Completed != 1 {
		t.Fatalf("北京上个月最后一秒确认的记录统计=%+v，want 本月已确认 0", stats)
	}
}
