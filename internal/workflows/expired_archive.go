package workflows

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

// ExpiredContractArchiveWorkflow 以 Temporal Cron 工作流启动。
// 日期依据 CON-002 按中国标准时间确定性派生。
func ExpiredContractArchiveWorkflow(ctx workflow.Context, input ExpiredArchiveInput) (ExpiredArchiveResult, error) {
	if input.AsOfDate == "" {
		chinaStandardTime := time.FixedZone("Asia/Shanghai", 8*60*60)
		input.AsOfDate = workflow.Now(ctx).In(chinaStandardTime).Format(time.DateOnly)
	}
	var result ExpiredArchiveResult
	err := executeLicensedActivity(ctx, activityContext(ctx), ActivityArchiveExpired, input, &result)
	return result, err
}
