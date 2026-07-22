package workflows

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
)

type memoryStore struct {
	mu        sync.Mutex
	commands  []ApprovalCommand
	completed []CompleteApprovalActivityInput
}

func (*memoryStore) StartApproval(context.Context, StartApprovalActivityInput) error { return nil }
func (s *memoryStore) RecordCommand(_ context.Context, in RecordCommandActivityInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, in.Command)
	return nil
}
func (s *memoryStore) CompleteApproval(_ context.Context, in CompleteApprovalActivityInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, in)
	return nil
}
func (*memoryStore) CreateNotification(context.Context, NotifyActivityInput) error { return nil }
func (*memoryStore) ArchiveExpired(context.Context, ExpiredArchiveInput) (ExpiredArchiveResult, error) {
	return ExpiredArchiveResult{Archived: 2}, nil
}

func TestContractApprovalWorkflowApprovesAllNodes(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	nodes := []approval.Node{
		{ID: "sales", AssigneeIDs: []string{"sales-user"}, Countersign: approval.CountersignAll},
		{ID: "tech", AssigneeIDs: []string{"tech-user"}, Countersign: approval.CountersignAll},
		{ID: "finance", AssigneeIDs: []string{"finance-user"}, Countersign: approval.CountersignAll},
	}
	for index, userID := range []string{"sales-user", "tech-user", "finance-user"} {
		index, userID := index, userID
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: userID, Action: ActionApprove, ActorUserID: userID, OccurredAt: time.Now().UTC()})
		}, time.Duration(index+1)*time.Minute)
	}
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 1, ApplicantUserID: "owner", ContentHash: "hash", Nodes: nodes, DefaultNodeTimeout: 72 * time.Hour, ReminderInterval: 24 * time.Hour})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusApproved, state.Status)
	require.Len(t, store.completed, 1)
	require.Equal(t, contract.StatusActive, store.completed[0].TargetStatus)
}

func TestContractApprovalWorkflowWithdraws(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "withdraw", Action: ActionWithdraw, ActorUserID: "owner", Comment: "需要修改条款", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 1, ApplicantUserID: "owner", ContentHash: "hash", Nodes: []approval.Node{{ID: "sales", AssigneeIDs: []string{"sales-user"}}}})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusWithdrawn, state.Status)
	require.Equal(t, contract.StatusDraft, store.completed[0].TargetStatus)
}

func TestStatusChangeWorkflowAppliesApprovedTarget(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "approve", Action: ActionApprove, ActorUserID: "admin", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 2, ApplicantUserID: "owner", FromStatus: contract.StatusActive, TargetStatus: contract.StatusInProgress, Reason: "开始履约", AdminUserIDs: []string{"admin"}, Timeout: 72 * time.Hour})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusApproved, state.Status)
	require.Equal(t, contract.StatusInProgress, store.completed[0].TargetStatus)
}

func TestContractApprovalWorkflowExpiresNode(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 1, ApplicantUserID: "owner", ContentHash: "hash", Nodes: []approval.Node{{ID: "sales", AssigneeIDs: []string{"sales-user"}}}, DefaultNodeTimeout: 3 * time.Hour, ReminderInterval: time.Hour})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusExpired, state.Status)
	require.Equal(t, contract.StatusDraft, store.completed[0].TargetStatus)
}

func TestEnhancedApprovalCommands(t *testing.T) {
	state := ApprovalState{Status: approval.StatusRunning, ApplicantUserID: "owner", CurrentNodeIndex: 1, Nodes: []RuntimeNode{
		{Node: approval.Node{ID: "sales", AssigneeIDs: []string{"sales"}}, Status: approval.NodeApproved, ApprovedBy: map[string]bool{"sales": true}},
		{Node: approval.Node{ID: "tech", AssigneeIDs: []string{"tech"}, Countersign: approval.CountersignAll}, Status: approval.NodeActive, ApprovedBy: map[string]bool{}},
	}}
	changed, terminal, err := applyContractCommand(&state, ApprovalCommand{Action: ActionAddSign, ActorUserID: "tech", TargetUserIDs: []string{"legal"}, Countersign: approval.CountersignAll})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, terminal)
	require.ElementsMatch(t, []string{"tech", "legal"}, state.Nodes[1].Node.AssigneeIDs)

	_, _, err = applyContractCommand(&state, ApprovalCommand{Action: ActionTransfer, ActorUserID: "tech", TargetUserIDs: []string{"tech-delegate"}})
	require.NoError(t, err)
	require.Contains(t, state.Nodes[1].Node.AssigneeIDs, "tech-delegate")

	_, _, err = applyContractCommand(&state, ApprovalCommand{Action: ActionReturn, ActorUserID: "tech-delegate", TargetNodeID: "sales"})
	require.NoError(t, err)
	require.Equal(t, 0, state.CurrentNodeIndex)
	require.Equal(t, approval.NodePending, state.Nodes[0].Status)
}
