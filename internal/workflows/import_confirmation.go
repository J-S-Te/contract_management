package workflows

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"time"
)

const ImportConfirmationWorkflowName = "contract-import-confirmation"

type ImportConfirmationInput struct {
	TenantID, BatchID, Digest string
	Revision                  uint64
}
type ImportConfirmationStore interface {
	AdvanceImportConfirmation(context.Context, ImportConfirmationInput) (string, error)
}

// ImportConfirmationWorkflow 独立调度历史确认，不生成历史普通审批事实。
// 参数：ctx为Temporal上下文，in为冻结修订和清单摘要。
// 返回值：持久确认终态或明确工作流错误。
func ImportConfirmationWorkflow(ctx workflow.Context, in ImportConfirmationInput) (string, error) {
	if in.BatchID == "" || in.Digest == "" || in.Revision == 0 {
		return "", temporal.NewNonRetryableApplicationError("invalid history identity", "IMPORT_CONFLICT", nil)
	}
	opts := workflow.ActivityOptions{StartToCloseTimeout: time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute, MaximumAttempts: 0}}
	ctx = workflow.WithActivityOptions(ctx, opts)
	for {
		var state string
		if err := executeLicensedActivity(ctx, ctx, "AdvanceImportConfirmation", in, &state); err != nil {
			return state, err
		}
		if state == "APPROVED" || state == "REJECTED" || state == "WITHDRAWN" || state == "EXPIRED" {
			return state, nil
		}
		if err := workflow.Sleep(ctx, 5*time.Second); err != nil {
			return state, err
		}
	}
}

// AdvanceImportConfirmation 复核并执行权威台账中的受信命令。
// 参数：ctx为活动上下文，in为独立确认身份。
// 返回值：当前确认状态或存储、许可和不可重试冲突。
func (a *Activities) AdvanceImportConfirmation(ctx context.Context, in ImportConfirmationInput) (string, error) {
	if err := a.checkLicense(ctx); err != nil {
		return "", err
	}
	store, ok := a.Store.(ImportConfirmationStore)
	if !ok {
		return "", temporal.NewNonRetryableApplicationError("history store unavailable", "IMPORT_UNSUPPORTED", nil)
	}
	state, err := store.AdvanceImportConfirmation(ctx, in)
	if errors.Is(err, contract.ErrImportManifest) {
		return state, temporal.NewNonRetryableApplicationError("history baseline conflict", "IMPORT_CONFLICT", nil)
	}
	return state, err
}
