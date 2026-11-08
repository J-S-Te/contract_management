package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"
	"testing"
	"time"
)

// AcceptStatusChange 为既有应用单测提供事务端口替身，真实锁与回滚由MySQL测试覆盖。
// 参数：ctx为请求上下文，in为审批输入，start为真实服务构造的启动回调。
// 返回值：启动回调错误；此替身不模拟持久层受理保证。
func (r *recordingRepository) AcceptStatusChange(ctx context.Context, _ workflows.StartApprovalActivityInput, start func(context.Context) (string, error)) error {
	_, err := start(ctx)
	return err
}

// TestStatusChangeStartRPCBudgetRespectsCaller 验证启动RPC不超过五秒及调用方期限。
// 参数：t为测试上下文，分别使用无限期与短期限调用方。
// 返回值：无；缺少RPC期限或扩大调用方期限时失败。
func TestStatusChangeStartRPCBudgetRespectsCaller(t *testing.T) {
	for _, callerTimeout := range []time.Duration{0, time.Second} {
		t.Run(callerTimeout.String(), func(t *testing.T) {
			ctx := context.Background()
			cancel := func() {}
			if callerTimeout > 0 {
				ctx, cancel = context.WithTimeout(ctx, callerTimeout)
			}
			defer cancel()
			repo := &recordingRepository{contract: statusAuthorizationContract("tenant-1", "sales-1", contract.StatusInProgress)}
			temporal := temporalmocks.NewClient(t)
			run := temporalmocks.NewWorkflowRun(t)
			run.On("GetID").Return("workflow-1")
			run.On("GetRunID").Return("run-1")
			temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.StatusChangeWorkflowName, mock.Anything).Return(func(startCtx context.Context, _ client.StartWorkflowOptions, _ any, _ ...any) (client.WorkflowRun, error) {
				deadline, ok := startCtx.Deadline()
				if !ok || time.Until(deadline) > 5*time.Second {
					t.Errorf("启动RPC没有五秒预算: deadline=%v exists=%v", deadline, ok)
				}
				if callerDeadline, ok := ctx.Deadline(); ok && deadline.After(callerDeadline) {
					t.Errorf("启动RPC超出调用方期限: rpc=%v caller=%v", deadline, callerDeadline)
				}
				return run, nil
			}).Once()
			service := &Service{Repo: repo, Personnel: &statusAuthorizationPersonnel{}, Temporal: temporal}
			if _, err := service.ChangeStatus(ctx, statusAuthorizationActor("sales"), "contract-1", 3, contract.StatusPendingPay, "申请收款"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
