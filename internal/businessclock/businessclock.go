// Package businessclock 是合同业务日历值的唯一时区来源。
//
// 业务方约定「业务上的时间必须按北京时间（UTC+8）」：合同单号的年份段、年度流水桶、
// 签署台账的“今天/本月”、经营看板的到期判定，全部按北京日历日计算。若这些地方直接
// 用 UTC 的 now 换算日历值，会在北京时间每天 00:00–08:00 之间整体算错一天/一月/一年
// ——单号年份段与 seq_year 属于写错数据，不只是显示问题。
//
// 边界刻意划清：
//   - 业务日历值（哪一年、哪一天、哪个月的第一天）一律走本包；
//   - 存储时间戳（created_at/updated_at/occurred_at/issued_at/locked_at/
//     next_attempt_at/lease_until/expires_at 等）与 TTL、token 过期等绝对时刻比较，
//     继续用 time.Now().UTC() 或原样时刻，不要改成北京时间写入。
//
// 时区来源优先加载 IANA 时区库（运行镜像已装 tzdata，见 Dockerfile 的 apk add tzdata），
// 加载失败（本地 go run、CI 精简环境、未来换 scratch/distroless 镜像）时回落到固定
// +08:00 的 CST 时区：北京自 1991 年起不再使用夏令时，两者在业务上等价，且回落保证
// 任何环境都不会 panic。
package businessclock

import (
	"sync"
	"time"
)

const (
	// fixedZoneName 是回落时区的名字。它固定 +08:00、不带 IANA 规则，叫 CST 比冒用
	// "Asia/Shanghai" 更诚实：名字本身就说明这里没有夏令时历史规则。
	fixedZoneName = "CST"
	// fixedZoneOffset 是北京时间的固定偏移（8 小时，单位秒）。
	fixedZoneOffset = 8 * 60 * 60
)

var location = sync.OnceValue(func() *time.Location {
	if loaded, err := time.LoadLocation("Asia/Shanghai"); err == nil && loaded != nil {
		return loaded
	}
	return time.FixedZone(fixedZoneName, fixedZoneOffset)
})

// Location 返回业务时区（北京时间 UTC+8）。
//
// 只读且进程内缓存：首次调用时尝试 IANA 时区库，失败则回落固定 +08:00。
// 返回值：永不为 nil 的时区对象。
func Location() *time.Location { return location() }

// Now 返回当前时刻，并已换算到业务时区。
//
// 仅用于推导业务日历值；需要落库的时间戳仍应用 time.Now().UTC()。
// 返回值：北京时区下的当前时刻。
func Now() time.Time { return time.Now().In(Location()) }

// DayStartOf 返回 t 所在业务日历日的 00:00（业务时区）。
// 参数：t 为任意时刻，可位于任意时区。
// 返回值：北京日历日当天的 00:00。
func DayStartOf(t time.Time) time.Time {
	local := t.In(Location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, Location())
}

// Today 返回业务意义上的“今天”，即北京当天 00:00。
// 返回值：北京时区下当天 00:00。
func Today() time.Time { return DayStartOf(time.Now()) }

// MonthStart 返回业务意义上的“本月第一天”，即北京本月 1 日 00:00。
// 返回值：北京时区下本月 1 日 00:00。
func MonthStart() time.Time { return MonthStartOf(time.Now()) }

// MonthStartOf 返回 t 所在业务月份的 1 日 00:00（业务时区）。
// 参数：t 为任意时刻，可位于任意时区。
// 返回值：北京本月 1 日 00:00。
func MonthStartOf(t time.Time) time.Time {
	local := t.In(Location())
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, Location())
}

// YearOf 返回 t 在业务时区下的年份，用于单号年份段与年度流水桶。
// 参数：t 为任意时刻（例如调用方已有的 UTC now）。
// 返回值：北京日历日的年份。
func YearOf(t time.Time) int { return t.In(Location()).Year() }

// CalendarDay 把 t 换算成业务日历日，并以“同一天的 00:00 UTC 墙钟”表示。
//
// 这是数据库参数专用口径：MySQL 驱动默认 loc=UTC，会把 time.Time 先转成 UTC 再格式化成
// 墙钟写入参数，而库里的 DATE 列（如 end_date）与由 Go 以 UTC 写入的 DATETIME 列都用
// UTC 墙钟保存。因此「北京今天」若直接把 +08:00 的 00:00 传进 SQL，会被换算成前一天
// 16:00 的参数，整条比较错开 8 小时。本函数输出的日期永远是纯日期比较的安全形式。
//
// 参数：t 为任意时刻。
// 返回值：业务日历日当天的 00:00（UTC 时区）。
func CalendarDay(t time.Time) time.Time {
	local := t.In(Location())
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// CalendarDayBefore 判断 a 的业务日历日是否早于 b 的业务日历日。
//
// 纯日期比较：忽略时刻与入参时区表示差异，语义与 SQL 的 `date_a < date_b` 一致，
// 用于“到期当天仍有效、次日才算过期”这类判定。
// 参数：a、b 为待比较时刻。
// 返回值：a 的北京日历日早于 b 的北京日历日时返回 true。
func CalendarDayBefore(a, b time.Time) bool { return CalendarDay(a).Before(CalendarDay(b)) }
