package mysql

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"testing"
	"time"
)

// TestApprovalTerminalClosesPendingTasksOnMySQL 验证拒绝、撤回及超时关闭全链待办且重提不混入旧轮次。
// 参数：t为隔离MySQL测试上下文。
// 返回值：无。
func TestApprovalTerminalClosesPendingTasksOnMySQL(t *testing.T) {
	for _, status := range []approval.Status{approval.StatusRejected, approval.StatusWithdrawn, approval.StatusExpired} {
		t.Run(string(status), func(t *testing.T) {
			repo, tenant := draftE2ERepository(t)
			item := stampedE2EContract(t, repo, tenant, contract.StatusDraft)
			ctx := context.Background()
			id := ulid.Make().String()
			start := workflows.StartApprovalActivityInput{ApprovalID: id, TenantID: tenant, ContractID: item.ID, ExpectedVersion: 1, Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending, ContentHash: item.ContentHash, ApplicantUserID: item.OwnerUserID, WorkflowID: "wf-terminal", RunID: "run-terminal", Nodes: []approval.Node{{ID: "one", AssigneeIDs: []string{"active-user"}, Countersign: approval.CountersignAny}, {ID: "two", AssigneeIDs: []string{"pending-user"}, Countersign: approval.CountersignAny}}}
			require.NoError(t, repo.StartApproval(ctx, start))
			require.NoError(t, repo.CompleteApproval(ctx, workflows.CompleteApprovalActivityInput{ApprovalID: id, TenantID: tenant, ContractID: item.ID, ActorUserID: "active-user", Status: status, WorkflowID: "wf-terminal"}))
			var count int64
			require.NoError(t, repo.db.Model(&approvalTaskRecord{}).Where("approval_id = ? AND status IN ?", id, assignedApprovalTaskStatuses()).Count(&count).Error)
			require.Zero(t, count, "terminal instances must close active and pending")
			// 历史脏待办也不得出现在列表。
			require.NoError(t, repo.db.Model(&approvalTaskRecord{}).Where("approval_id = ? AND node_id = ?", id, "two").Update("status", approval.NodePending).Error)
			tasks, err := repo.ListTasks(ctx, tenant, "pending-user", 50)
			require.NoError(t, err)
			require.Empty(t, tasks)
			current, err := repo.GetContract(ctx, tenant, item.ID)
			require.NoError(t, err)
			start.ApprovalID = ulid.Make().String()
			start.ExpectedVersion = current.Version
			start.WorkflowID = "wf-" + start.ApprovalID
			require.NoError(t, repo.StartApproval(ctx, start))
			tasks, err = repo.ListTasks(ctx, tenant, "pending-user", 50)
			require.NoError(t, err)
			require.Len(t, tasks, 1)
			require.Equal(t, start.ApprovalID, tasks[0].ApprovalID)
		})
	}
}

// TestStatusChangeTransferProjectsTasksOnMySQL 验证真实Workflow活动将转派投影到数据库且旧人不能审批。
// 参数：t为隔离MySQL和Temporal测试环境上下文。
// 返回值：无。
func TestStatusChangeTransferProjectsTasksOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusActive)
	id := ulid.Make().String()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&workflows.Activities{Store: repo})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.CommandSignalName, workflows.ApprovalCommand{CommandID: ulid.Make().String(), Action: workflows.ActionTransfer, ActorUserID: "old", TargetUserIDs: []string{"new"}, TargetNodeID: "admin", OriginNodeID: "admin", OriginNodeRevision: new(uint64), Comment: "handover", OccurredAt: time.Now()})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		tasks, err := repo.ListTasks(context.Background(), tenant, "new", 50)
		require.NoError(t, err)
		require.Len(t, tasks, 1)
		old, err := repo.ListTasks(context.Background(), tenant, "old", 50)
		require.NoError(t, err)
		require.Empty(t, old)
		env.SignalWorkflow(workflows.CommandSignalName, workflows.ApprovalCommand{CommandID: ulid.Make().String(), Action: workflows.ActionApprove, ActorUserID: "old", OccurredAt: time.Now()})
	}, 2*time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(workflows.CommandSignalName, workflows.ApprovalCommand{CommandID: ulid.Make().String(), Action: workflows.ActionReject, ActorUserID: "new", Comment: "reject", OccurredAt: time.Now()})
	}, 3*time.Minute)
	env.ExecuteWorkflow(workflows.StatusChangeWorkflow, workflows.StatusChangeInput{ApprovalID: id, TenantID: tenant, ContractID: item.ID, ContractVersion: 1, ApplicantUserID: item.OwnerUserID, AdminUserIDs: []string{"old"}, FromStatus: contract.StatusActive, TargetStatus: contract.StatusInProgress, Reason: "change"})
	require.NoError(t, env.GetWorkflowError())
	var state workflows.ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusRejected, state.Status)
	actions, err := repo.ListApprovalActions(context.Background(), tenant, id)
	require.NoError(t, err)
	require.Len(t, actions, 2)
}
