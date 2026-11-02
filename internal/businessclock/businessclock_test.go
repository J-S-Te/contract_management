package businessclock

import (
	"testing"
	"time"
)

// TestLocationIsBeijingOffset 验证业务时区固定为 UTC+8，且不依赖时区库是否可用。
func TestLocationIsBeijingOffset(t *testing.T) {
	location := Location()
	if location == nil {
		t.Fatal("Location() 返回 nil，业务时区来源不可用")
	}
	name, offset := time.Date(2026, 1, 1, 0, 0, 0, 0, location).Zone()
	if offset != 8*60*60 {
		t.Fatalf("业务时区偏移=%d 秒，want %d 秒（UTC+8）", offset, 8*60*60)
	}
	if name == "" {
		t.Fatal("业务时区缺少名字，Timezone 名不能为空")
	}
	// 同时刻在两个时区应指向同一绝对时刻，只是墙钟不同。
	instant := time.Date(2026, 8, 1, 3, 0, 0, 0, time.UTC)
	if !instant.Equal(instant.In(location)) {
		t.Fatal("换算到业务时区后绝对时刻发生变化")
	}
}

// TestTodayIsBeijingMidnight 验证 Today() 是北京当天 00:00。
func TestTodayIsBeijingMidnight(t *testing.T) {
	now := Now()
	today := Today()
	if hour, minute, second := today.Hour(), today.Minute(), today.Second(); hour != 0 || minute != 0 || second != 0 || today.Nanosecond() != 0 {
		t.Fatalf("Today()=%v 不是 00:00:00", today)
	}
	if today.Location() != Location() {
		t.Fatalf("Today() 时区=%v，want 业务时区 %v", today.Location(), Location())
	}
	local := now.In(Location())
	if today.Year() != local.Year() || today.Month() != local.Month() || today.Day() != local.Day() {
		t.Fatalf("Today()=%v 与北京当天 %v 不一致", today, local)
	}
}

// TestMonthStartIsFirstDayMidnight 验证 MonthStart() 是北京本月 1 日 00:00。
func TestMonthStartIsFirstDayMidnight(t *testing.T) {
	monthStart := MonthStart()
	if monthStart.Day() != 1 {
		t.Fatalf("MonthStart()=%v 不是本月 1 日", monthStart)
	}
	if hour, minute, second := monthStart.Hour(), monthStart.Minute(), monthStart.Second(); hour != 0 || minute != 0 || second != 0 {
		t.Fatalf("MonthStart()=%v 不是 00:00:00", monthStart)
	}
	local := Now().In(Location())
	if monthStart.Year() != local.Year() || monthStart.Month() != local.Month() {
		t.Fatalf("MonthStart()=%v 与北京本月 %v 不一致", monthStart, local)
	}
	// 下月边界必须落在北京时间，而不是 UTC：北京时间 2026-08-01 00:00 = UTC 2026-07-31 16:00。
	fixed := time.Date(2026, 8, 1, 0, 0, 0, 0, Location())
	if got, want := fixed.UTC(), time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("北京 8 月 1 日 00:00 换算到 UTC=%v，want %v", got, want)
	}
}

// TestYearOfUsesBeijingCalendar 验证 YearOf 按北京日历日换年，覆盖跨年 00:00–08:00 区间。
func TestYearOfUsesBeijingCalendar(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want int
	}{
		{name: "北京 2026-01-01 00:30（UTC 仍是 2025-12-31）", at: time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC), want: 2026},
		{name: "北京 2025-12-31 23:30", at: time.Date(2025, 12, 31, 15, 30, 0, 0, time.UTC), want: 2025},
		{name: "北京 2026-01-01 11:00", at: time.Date(2026, 1, 1, 3, 0, 0, 0, time.UTC), want: 2026},
		{name: "北京 2025-01-01 07:59:59（UTC 仍是 2024-12-31）", at: time.Date(2024, 12, 31, 23, 59, 59, 0, time.UTC), want: 2025},
		{name: "北京时间入参原样返回", at: time.Date(2026, 2, 3, 9, 0, 0, 0, Location()), want: 2026},
	}
	for _, item := range cases {
		if got := YearOf(item.at); got != item.want {
			t.Fatalf("%s: YearOf(%v)=%d，want %d", item.name, item.at, got, item.want)
		}
	}
}

// TestCalendarDayUsesBeijingDate 验证数据库参数口径：业务日历日以 UTC 0 点表示，
// 避免 +08:00 的 00:00 被驱动换算成前一天 16:00 而错开 8 小时。
func TestCalendarDayUsesBeijingDate(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{
			name: "UTC 2025-12-31T16:30 属于北京 2026-01-01",
			at:   time.Date(2025, 12, 31, 16, 30, 0, 0, time.UTC),
			want: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "UTC 2025-12-31T15:30 属于北京 2025-12-31",
			at:   time.Date(2025, 12, 31, 15, 30, 0, 0, time.UTC),
			want: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "北京时间入参取同一日历日",
			at:   time.Date(2026, 8, 1, 23, 59, 0, 0, Location()),
			want: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, item := range cases {
		got := CalendarDay(item.at)
		if !got.Equal(item.want) {
			t.Fatalf("%s: CalendarDay(%v)=%v，want %v", item.name, item.at, got, item.want)
		}
		if got.Location() != time.UTC {
			t.Fatalf("%s: CalendarDay 必须输出 UTC 墙钟，实际时区=%v", item.name, got.Location())
		}
	}
}

// TestCalendarDayBeforeKeepsDueDayValid 验证“到期当天仍有效、次日才算过期”的日期比较。
func TestCalendarDayBeforeKeepsDueDayValid(t *testing.T) {
	// 北京 2026-08-01 11:00 与北京 2026-08-02 00:30（UTC 仍在 8 月 1 日）是同一天判断的对照。
	today := Now()
	if CalendarDayBefore(today, today) {
		t.Fatal("同一天不应判定为“更早”")
	}
	dueToday := CalendarDay(today)
	if CalendarDayBefore(dueToday, today) {
		t.Fatal("到期当天仍应有效：到期日不早于今天")
	}
	if !CalendarDayBefore(dueToday.AddDate(0, 0, -1), today) {
		t.Fatal("到期日为昨天时必须判定为已过期")
	}
	// 入参时区表示不同、日历日相同：不能因为 8 小时偏移被判成更早。
	beijingMidnight := time.Date(2026, 8, 1, 0, 0, 0, 0, Location())
	utcMidnight := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if CalendarDayBefore(beijingMidnight, utcMidnight) || CalendarDayBefore(utcMidnight, beijingMidnight) {
		t.Fatal("同一业务日历日的不同时区表示不应互相判定为更早")
	}
}

// TestDayStartOfAndMonthStartOfUseBusinessZone 验证任意时刻都能换算到北京日/月边界。
func TestDayStartOfAndMonthStartOfUseBusinessZone(t *testing.T) {
	at := time.Date(2026, 3, 15, 20, 0, 0, 0, time.UTC) // 北京 2026-03-16 04:00
	day := DayStartOf(at)
	if day.Year() != 2026 || day.Month() != time.March || day.Day() != 16 || day.Hour() != 0 {
		t.Fatalf("DayStartOf(%v)=%v，want 北京 2026-03-16 00:00", at, day)
	}
	if day.Location() != Location() {
		t.Fatalf("DayStartOf 时区=%v，want 业务时区", day.Location())
	}
	month := MonthStartOf(at)
	if month.Year() != 2026 || month.Month() != time.March || month.Day() != 1 || month.Hour() != 0 {
		t.Fatalf("MonthStartOf(%v)=%v，want 北京 2026-03-01 00:00", at, month)
	}
}
