package workflows

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
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

type failingNotificationStore struct{ *memoryStore }

func (*failingNotificationStore) CreateNotification(context.Context, NotifyActivityInput) error {
	return errors.New("notification database unavailable")
}

// 通知 Activity 持续失败不能让已完成的审批工作流返回错误；业务状态必须继续收口。
func TestContractApprovalWorkflowContinuesWhenNotificationsFail(t *testing.T) {
	store := &failingNotificationStore{memoryStore: &memoryStore{}}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "approve", Action: ActionApprove, ActorUserID: "sales-user", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 1, ApplicantUserID: "owner", ContentHash: "hash", Nodes: []approval.Node{{ID: "sales", AssigneeIDs: []string{"sales-user"}}}})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusApproved, state.Status)
	require.Len(t, store.completed, 1)
}

// 业务冲突必须标记为 NonRetryable，不能进入无限重试。
func TestCriticalActivityErrorMarksBusinessConflictNonRetryable(t *testing.T) {
	for _, businessError := range []error{apperrors.ErrStateConflict, contract.ErrNumberExhausted, contract.ErrInvalidNumberRule} {
		err := criticalActivityError(businessError)
		var applicationErr *temporal.ApplicationError
		require.ErrorAs(t, err, &applicationErr)
		require.True(t, applicationErr.NonRetryable())
		require.NotEqual(t, commercialLicenseErrorType, applicationErr.Type())
	}
	// 数据库/网络类执行故障必须保持可重试。
	executionErr := errors.New("dial tcp 127.0.0.1:3306: connection reset by peer")
	if err := criticalActivityError(executionErr); !errors.Is(err, executionErr) {
		t.Fatalf("执行故障必须原样返回以便重试: %v", err)
	}
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

// BIZ-CONTRACT-03 工作流层兜底：申请人即使同时是状态变更审批人，其自批信号也被
// 忽略——不批准、不写入审批记录，工作流继续等待直到超时。
func TestStatusChangeWorkflowIgnoresApplicantSelfApprove(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "self-approve", Action: ActionApprove, ActorUserID: "owner", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 2, ApplicantUserID: "owner", FromStatus: contract.StatusActive, TargetStatus: contract.StatusInProgress, Reason: "开始履约", AdminUserIDs: []string{"owner"}, Timeout: 3 * time.Hour})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusExpired, state.Status)
	require.Equal(t, approval.NodeActive, state.Nodes[0].Status)
	require.Empty(t, store.recorded, "被忽略的自我批准不能写入审批命令记录")
	require.Len(t, store.completed, 1)
	require.Equal(t, approval.StatusExpired, store.completed[0].Status)
}

// 同一节点上的非申请人审批人（管理员）审批仍然正常通过并应用目标状态。
func TestStatusChangeWorkflowAllowsNonApplicantAdminApprove(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "approve", Action: ActionApprove, ActorUserID: "backup-admin", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 2, ApplicantUserID: "owner", FromStatus: contract.StatusActive, TargetStatus: contract.StatusInProgress, Reason: "开始履约", AdminUserIDs: []string{"owner", "backup-admin"}, Timeout: 72 * time.Hour})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusApproved, state.Status)
	require.Equal(t, approval.NodeApproved, state.Nodes[0].Status)
	require.Len(t, store.recorded, 1)
	require.Equal(t, "backup-admin", store.recorded[0].Command.ActorUserID)
	require.Len(t, store.completed, 1)
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

// BIZ-CONTRACT-03 分离职责：申请人同时是当前节点审批人时也不能自己批准，
// 命令被拒绝且节点保持未通过；非申请人审批人审批仍正常通过。
func TestApplicantCannotApproveOwnContractApproval(t *testing.T) {
	state := ApprovalState{
		Status: approval.StatusRunning, ApplicantUserID: "admin",
		Nodes: []RuntimeNode{{
			Node:       approval.Node{ID: "admin", RoleCode: "admin", AssigneeIDs: []string{"admin", "backup-admin"}, Countersign: approval.CountersignAny},
			Status:     approval.NodeActive,
			ApprovedBy: map[string]bool{},
		}},
	}

	changed, terminal, err := applyContractCommand(&state, ApprovalCommand{
		Action: ActionApprove, ActorUserID: "admin", OccurredAt: time.Now().UTC(),
	})
	require.EqualError(t, err, "applicant cannot approve own contract approval")
	require.False(t, changed)
	require.False(t, terminal)
	require.Equal(t, approval.NodeActive, state.Nodes[0].Status)
	require.Empty(t, state.Nodes[0].ApprovedBy)
	require.Equal(t, 0, state.CurrentNodeIndex)
	require.Equal(t, approval.StatusRunning, state.Status)

	// 同一节点上的其他审批人（非申请人）审批仍然正常通过。
	changed, terminal, err = applyContractCommand(&state, ApprovalCommand{
		Action: ActionApprove, ActorUserID: "backup-admin", OccurredAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, terminal)
	require.Equal(t, approval.NodeApproved, state.Nodes[0].Status)
	require.Equal(t, 1, state.CurrentNodeIndex)
}

// 申请人即使同时是节点审批人，撤回仍只允许本人执行并保持成功。
func TestApplicantWithdrawStillAllowedWhenAlsoAssignee(t *testing.T) {
	state := ApprovalState{
		Status: approval.StatusRunning, ApplicantUserID: "admin",
		Nodes: []RuntimeNode{{
			Node:       approval.Node{ID: "admin", RoleCode: "admin", AssigneeIDs: []string{"admin"}, Countersign: approval.CountersignAny},
			Status:     approval.NodeActive,
			ApprovedBy: map[string]bool{},
		}},
	}

	_, _, err := applyContractCommand(&state, ApprovalCommand{Action: ActionWithdraw, ActorUserID: "backup-admin"})
	require.EqualError(t, err, "only applicant may withdraw")
	require.Equal(t, approval.StatusRunning, state.Status)

	changed, terminal, err := applyContractCommand(&state, ApprovalCommand{Action: ActionWithdraw, ActorUserID: "admin", Comment: "需要修改条款"})
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, terminal)
	require.Equal(t, approval.StatusWithdrawn, state.Status)
	require.Equal(t, approval.NodeActive, state.Nodes[0].Status)
}

// 工作流层：申请人作为唯一审批人自行批准的命令被拒绝后不写入审批记录，
// 节点超时收口为 expired，合同不会生效。
func TestContractApprovalWorkflowIgnoresApplicantSelfApprove(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "self-approve", Action: ActionApprove, ActorUserID: "owner", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.ExecuteWorkflow(ContractApprovalWorkflow, ContractApprovalInput{ApprovalID: "approval", TenantID: "tenant", ContractID: "contract", ContractVersion: 1, ApplicantUserID: "owner", ContentHash: "hash", Nodes: []approval.Node{{ID: "admin", RoleCode: "admin", AssigneeIDs: []string{"owner"}, Countersign: approval.CountersignAny}}, DefaultNodeTimeout: 3 * time.Hour, ReminderInterval: time.Hour})
	require.NoError(t, env.GetWorkflowError())
	var state ApprovalState
	require.NoError(t, env.GetWorkflowResult(&state))
	require.Equal(t, approval.StatusExpired, state.Status)
	require.Equal(t, approval.NodeActive, state.Nodes[0].Status)
	require.Empty(t, store.recorded, "被拒绝的自我批准不能写入审批命令记录")
	require.Len(t, store.completed, 1)
	require.Equal(t, contract.StatusDraft, store.completed[0].TargetStatus)
}

// conflictImportStore 让导入确认活动稳定返回指定错误，并记录活动真实执行次数：
// 用于验证确定性业务冲突既被标记为 NonRetryable，也不会进入无限重试。
type conflictImportStore struct {
	*memoryStore
	err   error
	calls int
}

func (s *conflictImportStore) AdvanceImportConfirmation(context.Context, ImportConfirmationInput) (string, error) {
	s.calls++
	return "", s.err
}

// 历史导入确认的编号业务错误必须是运维可读的中文业务冲突且 NonRetryable。
// 冲突判定复用审批路径的 criticalActivityError，错误码按导入语义使用 IMPORT_CONFLICT。
func TestImportConfirmationActivityMarksBusinessConflictsNonRetryable(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantHint string
	}{
		{"number exhausted", fmt.Errorf("%w: 流水号 10000 超出 4 位流水的上限 9999，请调整流水位宽或更换前缀", contract.ErrNumberExhausted), "流水位宽"},
		{"invalid number rule", fmt.Errorf("%w: 流水位宽必须是 1 到 9 之间的整数", contract.ErrInvalidNumberRule), "编号规则"},
		{"number conflict", fmt.Errorf("%w: HT-20260001 已被本租户的另一份合同占用", contract.ErrNumberConflict), "唯一键冲突"},
		{"manifest conflict", contract.ErrImportManifest, "基线冲突"},
		{"state conflict", fmt.Errorf("%w: expected pending, got draft", apperrors.ErrStateConflict), "确定性业务冲突"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			store := &conflictImportStore{memoryStore: &memoryStore{}, err: testCase.err}
			_, err := (&Activities{Store: store}).AdvanceImportConfirmation(context.Background(), ImportConfirmationInput{TenantID: "tenant", BatchID: "batch", Digest: "digest", Revision: 1})
			var applicationErr *temporal.ApplicationError
			require.ErrorAs(t, err, &applicationErr)
			require.True(t, applicationErr.NonRetryable(), "导入业务冲突必须 NonRetryable")
			require.Equal(t, "IMPORT_CONFLICT", applicationErr.Type())
			require.Contains(t, applicationErr.Error(), testCase.wantHint)
			// 原始错误必须保留在 cause 链上，便于运维定位具体规则或批次。
			require.ErrorIs(t, err, testCase.err)
		})
	}

	// 数据库/网络等执行故障保持可重试，继续交给工作流的重试预算。
	executionErr := errors.New("dial tcp 127.0.0.1:3306: connection reset by peer")
	store := &conflictImportStore{memoryStore: &memoryStore{}, err: executionErr}
	_, err := (&Activities{Store: store}).AdvanceImportConfirmation(context.Background(), ImportConfirmationInput{TenantID: "tenant", BatchID: "batch", Digest: "digest", Revision: 1})
	require.False(t, temporal.IsApplicationError(err), "执行故障不能被包装成业务冲突")
	require.ErrorIs(t, err, executionErr)
}

// 导入确认工作流用无限重试策略兜底执行故障，但编号业务冲突必须一次就收口：
// 活动只执行一次、工作流立即失败，批次不会被无限重试卡在中间态。
func TestImportConfirmationWorkflowDoesNotRetryBusinessConflict(t *testing.T) {
	store := &conflictImportStore{memoryStore: &memoryStore{}, err: fmt.Errorf(
		"%w: 连续 50 次生成的合同单号均被全系统登记表占用，请调整流水位宽或更换前缀", contract.ErrNumberExhausted)}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	// 即使回归成可重试也不会让用例挂死，而是以超时失败收场。
	env.SetTestTimeout(5 * time.Second)
	env.RegisterActivity(&Activities{Store: store})
	env.ExecuteWorkflow(ImportConfirmationWorkflow, ImportConfirmationInput{TenantID: "tenant", BatchID: "batch", Digest: "digest", Revision: 1})
	require.True(t, env.IsWorkflowCompleted())
	var applicationErr *temporal.ApplicationError
	require.ErrorAs(t, env.GetWorkflowError(), &applicationErr)
	require.True(t, applicationErr.NonRetryable())
	require.Equal(t, "IMPORT_CONFLICT", applicationErr.Type())
	require.Equal(t, 1, store.calls, "编号业务冲突不能被无限重试")
}
