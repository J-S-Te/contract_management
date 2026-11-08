package workflows

import (
	"context"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

type terminalFailureStore struct{ *memoryStore }

// CompleteApproval 模拟业务收口事务在持久化终决议之后失败。
// 参数：ctx为活动上下文，in为真实工作流生成的终决议。
// 返回值：不可重试活动故障，用于让内存Temporal测试环境停止工作流。
func (s *terminalFailureStore) CompleteApproval(_ context.Context, _ CompleteApprovalActivityInput) error {
	return temporal.NewNonRetryableApplicationError("completion transaction unavailable", "test-stop", nil)
}

// TestStatusChangeTerminalDecisionSurvivesCompletionFailure 验证终决议先于业务收口持久化。
// 参数：t为测试上下文，使用既有Temporal测试环境而不连接真实服务。
// 返回值：无；停止时缺少恢复所需的持久化终决议时失败。
func TestStatusChangeTerminalDecisionSurvivesCompletionFailure(t *testing.T) {
	for _, action := range []CommandAction{ActionReject, ActionWithdraw} {
		t.Run(string(action), func(t *testing.T) {
			store := &terminalFailureStore{memoryStore: &memoryStore{}}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterActivity(&Activities{Store: store})
			actor := "admin"
			want := approval.StatusRejected
			if action == ActionWithdraw {
				actor = "owner"
				want = approval.StatusWithdrawn
			}
			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "terminal-command", Action: action, ActorUserID: actor, Comment: "终决议", OccurredAt: time.Now().UTC()})
			}, time.Minute)
			env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 2, ApplicantUserID: "owner", FromStatus: contract.StatusInProgress, TargetStatus: contract.StatusPendingPay, Reason: "业务变更", AdminUserIDs: []string{"admin"}, Timeout: 3 * time.Hour})
			require.Error(t, env.GetWorkflowError())
			require.Len(t, store.recorded, 1)
			require.Equal(t, want, store.recorded[0].State.Status)
		})
	}
}

type acceptanceCaptureStore struct {
	*memoryStore
	start StartApprovalActivityInput
}

// StartApproval 捕获工作流的初始化活动契约。
// 参数：ctx为活动上下文，in为实际初始化输入。
// 返回值：成功；业务持久化由专门的隔离MySQL测试覆盖。
func (s *acceptanceCaptureStore) StartApproval(_ context.Context, in StartApprovalActivityInput) error {
	s.start = in
	return nil
}

// TestStatusChangeLegacyInputKeepsInitializationContract 验证旧历史输入默认走原活动初始化路径。
// 参数：t为测试上下文，不含RequireExisting的新字段即代表旧输入。
// 返回值：无；旧输入意外要求预受理或改变正常审批结果时失败。
func TestStatusChangeLegacyInputKeepsInitializationContract(t *testing.T) {
	store := &acceptanceCaptureStore{memoryStore: &memoryStore{}}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "reject", Action: ActionReject, ActorUserID: "admin", Comment: "拒绝", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "legacy-approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 2, ApplicantUserID: "owner", FromStatus: contract.StatusInProgress, TargetStatus: contract.StatusPendingPay, Reason: "业务变更", AdminUserIDs: []string{"admin"}, Timeout: 3 * time.Hour})
	require.NoError(t, env.GetWorkflowError())
	require.False(t, store.start.RequireExisting)
	require.Equal(t, "legacy-approval", store.start.ApprovalID)
	require.Len(t, store.completed, 1)
	require.Equal(t, approval.StatusRejected, store.completed[0].Status)
}
