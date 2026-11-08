package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	temporalmocks "go.temporal.io/sdk/mocks"
	"testing"
)

// TestCommandRequiresOriginalTaskIdentity 验证缺少或陈旧任务身份在Signal之前拒绝。
// 参数：t为测试上下文。
// 返回值：无。
func TestCommandRequiresOriginalTaskIdentity(t *testing.T) {
	for _, test := range []struct {
		name, origin string
		revision     *uint64
		valid        bool
	}{
		{"missing", "", nil, false}, {"wrong-node", "old", new(uint64), false}, {"old-revision", "node", new(uint64), false}, {"current", "node", approvalRevision(3), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := workflows.ApprovalState{Status: approval.StatusRunning, Nodes: []workflows.RuntimeNode{{Node: approval.Node{ID: "node", AssigneeIDs: []string{"reviewer"}}, Status: approval.NodeActive, Revision: 3}}}
			temporal := temporalmocks.NewClient(t)
			temporal.On("QueryWorkflow", mock.Anything, "wf", "run", workflows.StateQueryName).Return(encodedApprovalState(t, state), nil)
			var signals []workflows.ApprovalCommand
			if test.valid {
				temporal.On("SignalWorkflow", mock.Anything, "wf", "run", workflows.CommandSignalName, mock.Anything).Run(func(args mock.Arguments) { signals = append(signals, args.Get(4).(workflows.ApprovalCommand)) }).Return(nil)
			}
			repo := &recordingRepository{approvalMeta: approval.Meta{ID: "approval", TenantID: "tenant", ContractID: "contract", Status: approval.StatusRunning, WorkflowID: "wf", RunID: "run", ApplicantUserID: "owner"}, contract: contract.Contract{ID: "contract", TenantID: "tenant", OwnerUserID: "owner"}}
			service := &Service{Repo: repo, Temporal: temporal}
			actor := Principal{TenantID: "tenant", UserID: "reviewer", Permissions: map[string]bool{"approval.process": true, "contract.read": true}, PermissionScopes: allowAllScope("contract.read")}
			command := workflows.ApprovalCommand{Action: workflows.ActionApprove, OriginNodeID: test.origin, OriginNodeRevision: test.revision}
			id, err := service.Command(context.Background(), actor, "approval", command)
			if !test.valid {
				require.Error(t, err)
				require.Empty(t, id)
				return
			}
			require.NoError(t, err)
			again, err := service.Command(context.Background(), actor, "approval", command)
			require.NoError(t, err)
			require.Equal(t, id, again)
			require.Len(t, signals, 2)
			require.Equal(t, signals[0].CommandID, signals[1].CommandID)
		})
	}
}

// approvalRevision 构造测试所需的显式节点代次。
// 参数：revision为代次值。
// 返回值：该值的独立指针。
func approvalRevision(revision uint64) *uint64 { return &revision }

// TestCommandReplaysConsumedIdentityAfterNodeAdvance 验证消费后网络重试返回原身份且不再次发送。
// 参数：t为测试上下文。
// 返回值：无。
func TestCommandReplaysConsumedIdentityAfterNodeAdvance(t *testing.T) {
	command := workflows.ApprovalCommand{Action: workflows.ActionApprove, ActorUserID: "reviewer", OriginNodeID: "old", OriginNodeRevision: new(uint64)}
	id := approvalCommandID("tenant", "approval", command)
	state := workflows.ApprovalState{Status: approval.StatusRunning, ProcessedCommandIDs: []string{id}, Nodes: []workflows.RuntimeNode{{Node: approval.Node{ID: "new", AssigneeIDs: []string{"reviewer"}}, Status: approval.NodeActive}}}
	temporal := temporalmocks.NewClient(t)
	temporal.On("QueryWorkflow", mock.Anything, "wf", "run", workflows.StateQueryName).Return(encodedApprovalState(t, state), nil)
	repo := &recordingRepository{approvalMeta: approval.Meta{ID: "approval", TenantID: "tenant", ContractID: "contract", Status: approval.StatusRunning, WorkflowID: "wf", RunID: "run", ApplicantUserID: "owner"}, contract: contract.Contract{ID: "contract", TenantID: "tenant", OwnerUserID: "owner"}}
	service := &Service{Repo: repo, Temporal: temporal}
	actor := Principal{TenantID: "tenant", UserID: "reviewer", Permissions: map[string]bool{"approval.process": true, "contract.read": true}, PermissionScopes: allowAllScope("contract.read")}
	actual, err := service.Command(context.Background(), actor, "approval", command)
	require.NoError(t, err)
	require.Equal(t, id, actual)
	temporal.AssertNotCalled(t, "SignalWorkflow", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}
