package workflows

import (
	"time"

	"github.com/j-s-te/contract-management/internal/businessclock"
	"go.temporal.io/sdk/workflow"
)

// ExpiredContractArchiveWorkflow 以 Temporal Cron 工作流启动。
// 日期依据 CON-002 按中国标准时间（北京业务日）确定性派生。
//
// 业务时区统一取 businessclock.Location()：优先 IANA Asia/Shanghai，加载不到时回落到
// 固定 +08:00 的 CST。这里只推导业务日历日，绝不能用 UTC 的 workflow.Now 直接格式化，
// 否则北京时间每天 00:00–08:00 会算成前一天。Cron 表达式本身仍是 UTC 表达式
// （默认 0 16 * * * = 北京 00:00），不要按北京时间理解。
func ExpiredContractArchiveWorkflow(ctx workflow.Context, input ExpiredArchiveInput) (ExpiredArchiveResult, error) {
	if input.AsOfDate == "" {
		input.AsOfDate = workflow.Now(ctx).In(businessclock.Location()).Format(time.DateOnly)
	}
	var result ExpiredArchiveResult
	err := executeLicensedActivity(ctx, activityContext(ctx), ActivityArchiveExpired, input, &result)
	return result, err
}
