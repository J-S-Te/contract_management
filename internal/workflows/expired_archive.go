package workflows

import (
	"time"

	"go.temporal.io/sdk/workflow"
)

// ExpiredContractArchiveWorkflow is started as a Temporal Cron Workflow. The
// date is derived deterministically in China Standard Time for CON-002.
func ExpiredContractArchiveWorkflow(ctx workflow.Context, input ExpiredArchiveInput) (ExpiredArchiveResult, error) {
	if input.AsOfDate == "" {
		chinaStandardTime := time.FixedZone("Asia/Shanghai", 8*60*60)
		input.AsOfDate = workflow.Now(ctx).In(chinaStandardTime).Format(time.DateOnly)
	}
	var result ExpiredArchiveResult
	err := workflow.ExecuteActivity(activityContext(ctx), ActivityArchiveExpired, input).Get(ctx, &result)
	return result, err
}
