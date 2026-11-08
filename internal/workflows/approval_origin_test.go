package workflows

import (
	"encoding/json"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"testing"
	"time"
)

// TestApprovalOriginRejectsLateCommands 验证同一审批人连续节点不会消费旧任务命令。
// 参数：t为测试上下文。
// 返回值：无。
func TestApprovalOriginRejectsLateCommands(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		revision := uint64(0)
		command := ApprovalCommand{CommandID: "first", Action: ActionApprove, ActorUserID: "reviewer", OriginNodeID: "one", OriginNodeRevision: &revision}
		env.SignalWorkflow(CommandSignalName, command)
		env.SignalWorkflow(CommandSignalName, command)
		command.CommandID = "late-new-id"
		env.SignalWorkflow(CommandSignalName, command)
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "bad", Action: ActionReject, ActorUserID: "intruder", Comment: "illegal"})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "stop", Action: ActionWithdraw, ActorUserID: "owner", Comment: "stop"})
	}, 2*time.Minute)
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", ContractID: "contract", ApplicantUserID: "owner", Nodes: []approval.Node{{ID: "one", AssigneeIDs: []string{"reviewer"}, Countersign: approval.CountersignAny}, {ID: "two", AssigneeIDs: []string{"reviewer"}, Countersign: approval.CountersignAny}}})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusWithdrawn, state.Status)
	require.Equal(t, 1, state.CurrentNodeIndex)
	require.Len(t, store.recorded, 2)
}

// TestStatusChangeTransferUsesRuntimeAssignees 验证转派去重、实际新旧处理人权限及申请人隔离。
// 参数：t为测试上下文。
// 返回值：无。
func TestStatusChangeTransferUsesRuntimeAssignees(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		transfer := ApprovalCommand{CommandID: "transfer", Action: ActionTransfer, ActorUserID: "old", TargetUserIDs: []string{"new"}, Comment: "handover", TargetNodeID: "admin", OriginNodeID: "admin", OriginNodeRevision: new(uint64)}
		env.SignalWorkflow(CommandSignalName, transfer)
		env.SignalWorkflow(CommandSignalName, transfer)
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "old-approve", Action: ActionApprove, ActorUserID: "old"})
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "own-approve", Action: ActionApprove, ActorUserID: "owner"})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "new-approve", Action: ActionApprove, ActorUserID: "new"})
	}, 2*time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "approval", ContractID: "contract", ApplicantUserID: "owner", AdminUserIDs: []string{"old", "owner"}, FromStatus: "active", TargetStatus: "in_progress", Reason: "change"})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusApproved, state.Status)
	require.Len(t, store.recorded, 2)
	require.Equal(t, ActionTransfer, store.recorded[0].Command.Action)
	require.Equal(t, "new", store.recorded[1].Command.ActorUserID)
	require.Equal(t, []string{"new", "owner"}, store.recorded[0].State.Nodes[0].Node.AssigneeIDs)
}

// TestTaskRevisionRejectsReturnedAndTransferredOldTasks 验证相同节点ID重入及人员转派回原人仍隔离旧命令。
// 参数：t为测试上下文。
// 返回值：无。
func TestTaskRevisionRejectsReturnedAndTransferredOldTasks(t *testing.T) {
	state := ApprovalState{ApplicantUserID: "owner", Status: approval.StatusRunning, CurrentNodeIndex: 1, Nodes: initializeNodes([]approval.Node{{ID: "one", AssigneeIDs: []string{"reviewer"}, Countersign: approval.CountersignAny}, {ID: "two", AssigneeIDs: []string{"reviewer"}, Countersign: approval.CountersignAny}})}
	state.Nodes[0].Status = approval.NodeApproved
	state.Nodes[1].Status = approval.NodeActive
	zero := uint64(0)
	_, _, err := applyContractCommand(&state, ApprovalCommand{Action: ActionReturn, ActorUserID: "reviewer", TargetNodeID: "one", OriginNodeID: "two", OriginNodeRevision: &zero})
	require.NoError(t, err)
	_, _, err = applyContractCommand(&state, ApprovalCommand{Action: ActionApprove, ActorUserID: "reviewer", OriginNodeID: "one", OriginNodeRevision: &zero})
	require.Error(t, err, "old task must not approve the returned node")
	revision := state.Nodes[0].Revision
	_, _, err = applyContractCommand(&state, ApprovalCommand{Action: ActionTransfer, ActorUserID: "reviewer", TargetUserIDs: []string{"delegate"}, OriginNodeID: "one", OriginNodeRevision: &revision})
	require.NoError(t, err)
	_, _, err = applyContractCommand(&state, ApprovalCommand{Action: ActionTransfer, ActorUserID: "delegate", TargetUserIDs: []string{"reviewer"}})
	require.NoError(t, err)
	_, _, err = applyContractCommand(&state, ApprovalCommand{Action: ActionApprove, ActorUserID: "reviewer", OriginNodeID: "one", OriginNodeRevision: &revision})
	require.Error(t, err, "old task must not approve after assignment returns to original user")
}

// TestHistoricalStatusChangeTransferStaysIgnored 验证历史无来源转派仍被忽略且不插入额外活动。
// 参数：t为Temporal测试上下文。
// 返回值：无。
func TestHistoricalStatusChangeTransferStaysIgnored(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "historical-transfer", Action: ActionTransfer, ActorUserID: "old", TargetUserIDs: []string{"new"}, TargetNodeID: "admin", Comment: "historically ignored"})
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "historical-approve", Action: ActionApprove, ActorUserID: "old"})
	}, time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "approval", ContractID: "contract", ApplicantUserID: "owner", AdminUserIDs: []string{"old"}, FromStatus: "active", TargetStatus: "in_progress", Reason: "change"})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusApproved, state.Status)
	require.Len(t, store.recorded, 1, "historical transfer must not insert RecordCommand")
	require.Equal(t, "historical-approve", store.recorded[0].Command.CommandID)
	require.Equal(t, []string{"old"}, state.Nodes[0].Node.AssigneeIDs)
	require.Empty(t, state.ProcessedCommandIDs, "historical status commands preserve activity state shape")
	payload, err := json.Marshal(store.recorded[0].State)
	require.NoError(t, err)
	require.NotContains(t, string(payload), `"revision"`)
}

// TestHistoricalContractCommandsPreserveNodeShape 验证旧合同转派、批准及退回不新增任务代次字段。
// 参数：t为测试上下文。
// 返回值：无。
func TestHistoricalContractCommandsPreserveNodeShape(t *testing.T) {
	state := ApprovalState{ApplicantUserID: "owner", Status: approval.StatusRunning, Nodes: initializeNodes([]approval.Node{{ID: "one", AssigneeIDs: []string{"old"}, Countersign: approval.CountersignAny}, {ID: "two", AssigneeIDs: []string{"next"}, Countersign: approval.CountersignAny}})}
	for _, command := range []ApprovalCommand{
		{Action: ActionTransfer, ActorUserID: "old", TargetUserIDs: []string{"new"}},
		{Action: ActionApprove, ActorUserID: "new"},
		{Action: ActionReturn, ActorUserID: "next", TargetNodeID: "one"},
	} {
		_, _, err := applyContractCommand(&state, command)
		require.NoError(t, err)
		payload, err := json.Marshal(state)
		require.NoError(t, err)
		require.NotContains(t, string(payload), `"revision"`)
	}
}
