package application

import (
	"context"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/api/enums/v1"
)

// TestRecoverApprovalAcceptsUnappliedTerminalDecision 覆盖终决议已写入但业务事务未完成。
// 参数：t为测试上下文，覆盖普通审批与状态审批的各类终决议。
// 返回值：无；终态未应用被误判已完成或目标状态错误时失败。
func TestRecoverApprovalAcceptsUnappliedTerminalDecision(t *testing.T) {
	for _, kind := range []approval.Kind{approval.KindContract, approval.KindStatusChange} {
		for _, status := range []approval.Status{approval.StatusApproved, approval.StatusRejected, approval.StatusWithdrawn, approval.StatusExpired} {
			t.Run(string(kind)+"/"+string(status), func(t *testing.T) {
				state := workflows.ApprovalState{Status: status}
				service, repo := recoveryService(t, enums.WORKFLOW_EXECUTION_STATUS_FAILED, state)
				repo.approvalMeta.Status, repo.approvalMeta.Kind = status, kind
				repo.approvalMeta.FromStatus, repo.approvalMeta.TargetStatus = string(contract.StatusInProgress), string(contract.StatusPendingPay)
				if err := service.RecoverApproval(context.Background(), recoveryActor(), "approval-1"); err != nil {
					t.Fatalf("半完成终决议未能恢复: %v", err)
				}
				target := contract.StatusDraft
				if kind == approval.KindStatusChange {
					target = contract.StatusInProgress
				}
				if status == approval.StatusApproved {
					target = contract.StatusActive
					if kind == approval.KindStatusChange {
						target = contract.StatusPendingPay
					}
				}
				if repo.completed == nil || repo.completed.Status != status || repo.completed.TargetStatus != target {
					t.Fatalf("错误收口: %+v want status=%s target=%s", repo.completed, status, target)
				}
			})
		}
	}
}

// TestRecoverApprovalRejectsUnprovenStopAndUnresolvedNodes 验证恢复不猜测执行停止与节点结果。
// 参数：t为测试上下文，覆盖未知、继续新运行和节点索引不可信组合。
// 返回值：无；任何不可信终态被应用时失败。
func TestRecoverApprovalRejectsUnprovenStopAndUnresolvedNodes(t *testing.T) {
	cases := []struct {
		name      string
		execution enums.WorkflowExecutionStatus
		state     workflows.ApprovalState
	}{
		{"unknown_execution", enums.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED, workflows.ApprovalState{Status: approval.StatusRejected}},
		{"continued_execution", enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW, workflows.ApprovalState{Status: approval.StatusRejected}},
		{"unresolved_last_node", enums.WORKFLOW_EXECUTION_STATUS_FAILED, workflows.ApprovalState{Status: approval.StatusRunning, CurrentNodeIndex: 1, Nodes: []workflows.RuntimeNode{{Status: approval.NodeActive}}}},
		{"empty_nodes", enums.WORKFLOW_EXECUTION_STATUS_FAILED, workflows.ApprovalState{Status: approval.StatusRunning}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, repo := recoveryService(t, tc.execution, tc.state)
			if err := service.RecoverApproval(context.Background(), recoveryActor(), "approval-1"); err == nil || repo.completed != nil {
				t.Fatalf("不可信状态被收口: err=%v completed=%+v", err, repo.completed)
			}
		})
	}
}
