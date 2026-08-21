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
	mu            sync.Mutex
	commands      []ApprovalCommand
	recorded      []RecordCommandActivityInput
	completed     []CompleteApprovalActivityInput
	notifications []NotifyActivityInput
}

func (*memoryStore) StartApproval(context.Context, StartApprovalActivityInput) error { return nil }
func (s *memoryStore) RecordCommand(_ context.Context, in RecordCommandActivityInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, in.Command)
	s.recorded = append(s.recorded, in)
	return nil
}
func (s *memoryStore) CompleteApproval(_ context.Context, in CompleteApprovalActivityInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completed = append(s.completed, in)
	return nil
}
func (s *memoryStore) CreateNotification(_ context.Context, in NotifyActivityInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifications = append(s.notifications, in)
	return nil
}
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
	require.Len(t, store.recorded, 3)
	require.Equal(t, approval.NodeActive, store.recorded[0].State.Nodes[1].Status)
	require.False(t, store.recorded[0].State.Nodes[1].StartedAt.IsZero())
	require.Equal(t, approval.NodeActive, store.recorded[1].State.Nodes[2].Status)
	require.False(t, store.recorded[1].State.Nodes[2].StartedAt.IsZero())
	require.Equal(t, []string{"sales-user"}, store.notifications[0].Recipients)
	require.Equal(t, []string{"tech-user"}, store.notifications[1].Recipients)
	require.Equal(t, []string{"finance-user"}, store.notifications[2].Recipients)
	require.Equal(t, []string{"owner"}, store.notifications[3].Recipients)
	require.Equal(t, []string{"contract_specialist"}, store.notifications[4].RoleRecipients)
	require.Equal(t, "signing_pending", store.notifications[4].Type)
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

func TestContractApprovalWorkflowNotifiesAssignedUser(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "add-sign-1", Action: ActionAddSign, ActorUserID: "tech", TargetUserIDs: []string{"legal"}, Countersign: approval.CountersignAll, Comment: "需要法务会签", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "legal-approve", Action: ActionApprove, ActorUserID: "legal", OccurredAt: time.Now().UTC()})
	}, 2*time.Minute)
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 1, ApplicantUserID: "owner", ContentHash: "hash", Nodes: []approval.Node{{ID: "tech", AssigneeIDs: []string{"tech"}, Countersign: approval.CountersignAll}}})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, store.notifications, 4)
	require.Equal(t, []string{"tech"}, store.notifications[0].Recipients)
	require.Equal(t, []string{"legal"}, store.notifications[1].Recipients)
	require.Equal(t, "approval_assigned", store.notifications[1].Type)
	require.Equal(t, "approval:assigned:add-sign-1", store.notifications[1].DedupeKey)
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

func TestAnySignNodeAdvancesAfterOneOfMultipleAssigneesApproves(t *testing.T) {
	state := ApprovalState{
		Status: approval.StatusRunning,
		Nodes: []RuntimeNode{{
			Node: approval.Node{
				ID: "sales", AssigneeIDs: []string{"director-1", "director-2"},
				Countersign: approval.CountersignAny,
			},
			Status: approval.NodeActive, ApprovedBy: map[string]bool{},
		}},
	}

	changed, terminal, err := applyContractCommand(&state, ApprovalCommand{
		Action: ActionApprove, ActorUserID: "director-1", OccurredAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, terminal)
	require.Equal(t, approval.NodeApproved, state.Nodes[0].Status)
	require.Equal(t, 1, state.CurrentNodeIndex)
}

func TestRoleNodeWithLegacyAllSignAdvancesAfterOneDirectorApproves(t *testing.T) {
	state := ApprovalState{
		Status: approval.StatusRunning,
		Nodes: []RuntimeNode{{
			Node: approval.Node{
				ID: "finance-director", RoleCode: "finance_director",
				AssigneeIDs: []string{"finance-director-1", "finance-director-2"},
				Countersign: approval.CountersignAll,
			},
			Status: approval.NodeActive, ApprovedBy: map[string]bool{},
		}},
	}

	changed, terminal, err := applyContractCommand(&state, ApprovalCommand{
		Action: ActionApprove, ActorUserID: "finance-director-1", RoleNodeOrSign: true, OccurredAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, terminal)
	require.Equal(t, approval.NodeApproved, state.Nodes[0].Status)
	require.Equal(t, 1, state.CurrentNodeIndex)
}

func TestAddedSignerApprovalAdvancesToNextNode(t *testing.T) {
	state := ApprovalState{Status: approval.StatusRunning, Nodes: []RuntimeNode{
		{
			Node:   approval.Node{ID: "review", RoleCode: "tech_director", AssigneeIDs: []string{"director-1", "director-2"}, Countersign: approval.CountersignAny},
			Status: approval.NodeActive, ApprovedBy: map[string]bool{},
		},
		{
			Node:   approval.Node{ID: "finance", RoleCode: "finance_director", AssigneeIDs: []string{"finance"}, Countersign: approval.CountersignAny},
			Status: approval.NodePending, ApprovedBy: map[string]bool{},
		},
	}}

	changed, terminal, err := applyContractCommand(&state, ApprovalCommand{Action: ActionAddSign, ActorUserID: "director-1", TargetUserIDs: []string{"added"}, Countersign: approval.CountersignAll})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, terminal)
	require.ElementsMatch(t, []string{"director-1", "director-2", "added"}, state.Nodes[0].Node.AssigneeIDs)
	require.True(t, state.Nodes[0].ApprovedBy["director-1"])

	changed, terminal, err = applyContractCommand(&state, ApprovalCommand{Action: ActionApprove, ActorUserID: "added", OccurredAt: time.Now().UTC()})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, terminal)
	require.Equal(t, approval.NodeApproved, state.Nodes[0].Status)
	require.Equal(t, 1, state.CurrentNodeIndex)
	require.False(t, state.Nodes[0].ApprovedBy["director-2"])
}
