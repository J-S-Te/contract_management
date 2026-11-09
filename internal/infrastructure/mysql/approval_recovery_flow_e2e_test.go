package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	temporalmocks "go.temporal.io/sdk/mocks"
)

// TestStatusChangeSameVersionNewRoundOnMySQL 验证终决议后同版本可新申请且在途不可重复。
// 参数：t为测试上下文，仅使用显式隔离的CONTRACT_TEST_DSN。
// 返回值：无；终决议未释放防重键或在途重复被接受时失败。
func TestStatusChangeSameVersionNewRoundOnMySQL(t *testing.T) {
	for _, status := range []approval.Status{approval.StatusRejected, approval.StatusWithdrawn, approval.StatusExpired} {
		t.Run(string(status), func(t *testing.T) {
			repo, tenant := draftE2ERepository(t)
			item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
			in := recoveryFlowStart(tenant, item, approval.KindStatusChange)
			if err := repo.StartApproval(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if err := repo.StartApproval(context.Background(), in); err != nil {
				t.Fatalf("同次活动重试不幂等: %v", err)
			}
			duplicate := in
			duplicate.ApprovalID = ulid.Make().String()
			duplicate.WorkflowID = "recovery-" + duplicate.ApprovalID
			duplicate.RunID = "run-" + duplicate.ApprovalID
			if err := repo.StartApproval(context.Background(), duplicate); !errors.Is(err, apperrors.ErrStateConflict) {
				t.Fatalf("在途重复未被拒绝: %v", err)
			}
			if err := repo.CompleteApproval(context.Background(), workflows.CompleteApprovalActivityInput{ApprovalID: in.ApprovalID, TenantID: tenant, ContractID: item.ID, Status: status}); err != nil {
				t.Fatal(err)
			}
			current, err := repo.GetContract(context.Background(), tenant, item.ID)
			if err != nil || current.Version != item.Version || current.Status != item.Status {
				t.Fatalf("拒绝审批改变合同: %+v err=%v", current, err)
			}
			if err := repo.StartApproval(context.Background(), duplicate); err != nil {
				t.Fatalf("同版本新轮次不能启动: %v", err)
			}
		})
	}
}

// recoveryFlowStart 构造真实合同行对应的审批启动输入。
// 参数：tenant为隔离租户，item为测试合同，kind为审批类型。
// 返回值：包含独立审批ID及工作流身份的输入。
func recoveryFlowStart(tenant string, item contract.Contract, kind approval.Kind) workflows.StartApprovalActivityInput {
	id := ulid.Make().String()
	target := contract.StatusPendingPay
	if kind == approval.KindContract {
		target = contract.StatusPending
	}
	return workflows.StartApprovalActivityInput{ApprovalID: id, TenantID: tenant, ContractID: item.ID, ExpectedVersion: item.Version,
		Kind: kind, FromStatus: item.Status, TargetStatus: target, ContentHash: item.ContentHash, ApplicantUserID: item.OwnerUserID,
		WorkflowID: "recovery-" + id, RunID: "run-" + id, Nodes: []approval.Node{{ID: "admin", AssigneeIDs: []string{"admin-1"}, Countersign: approval.CountersignAny}}}
}

// TestTerminalApprovalRecoveryRealMetaOnMySQL 验证真实元数据与终态半完成恢复。
// 参数：t为测试上下文，仅连接隔离库并用Temporal客户端替身报告已停止。
// 返回值：无；终态不能恢复、元数据漏读完成标记或重复产生副作用时失败。
func TestTerminalApprovalRecoveryRealMetaOnMySQL(t *testing.T) {
	for _, kind := range []approval.Kind{approval.KindStatusChange, approval.KindContract} {
		for _, status := range []approval.Status{approval.StatusRejected, approval.StatusWithdrawn} {
			t.Run(string(kind)+"/"+string(status), func(t *testing.T) {
				repo, tenant := draftE2ERepository(t)
				initial := contract.StatusInProgress
				if kind == approval.KindContract {
					initial = contract.StatusDraft
				}
				item := stampedE2EContract(t, repo, tenant, initial)
				start := recoveryFlowStart(tenant, item, kind)
				if err := repo.StartApproval(context.Background(), start); err != nil {
					t.Fatal(err)
				}
				state := workflows.ApprovalState{ApprovalID: start.ApprovalID, Kind: kind, Status: status, ContractID: item.ID}
				command := workflows.RecordCommandActivityInput{ApprovalID: start.ApprovalID, TenantID: tenant, ContractID: item.ID, State: state,
					Command: workflows.ApprovalCommand{CommandID: ulid.Make().String(), Action: workflows.ActionReject, ActorUserID: "admin-1", Comment: "拒绝"}}
				if err := repo.RecordCommand(context.Background(), command); err != nil {
					t.Fatal(err)
				}
				temporal := temporalmocks.NewClient(t)
				temporal.On("DescribeWorkflowExecution", mock.Anything, start.WorkflowID, start.RunID).Return(&workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_FAILED}}, nil).Maybe()
				service := &application.Service{Repo: repo, Temporal: temporal}
				actor := application.Principal{TenantID: tenant, UserID: "admin-1", Permissions: map[string]bool{"approval.manage": true}}
				if err := service.RecoverApproval(context.Background(), actor, start.ApprovalID); err != nil {
					t.Fatalf("真实terminal+未应用恢复失败: %v", err)
				}
				meta, err := repo.GetApprovalMeta(context.Background(), tenant, start.ApprovalID)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(meta)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				if fields["completion_applied"] != true {
					t.Fatalf("真实Meta遗漏完成标记: %s", raw)
				}
				if err := service.RecoverApproval(context.Background(), actor, start.ApprovalID); err != nil {
					t.Fatalf("重复恢复应无副作用: %v", err)
				}
			})
		}
	}
}

// TestApprovalMetaExposesAppliedFactOnMySQL 验证真实仓储把完成事实传递给恢复调用方。
// 参数：t为测试上下文，仅使用隔离MySQL。
// 返回值：无；完成前后元数据缺失或错误读取completion_applied时失败。
func TestApprovalMetaExposesAppliedFactOnMySQL(t *testing.T) {
	repo, tenant := draftE2ERepository(t)
	item := stampedE2EContract(t, repo, tenant, contract.StatusInProgress)
	start := recoveryFlowStart(tenant, item, approval.KindStatusChange)
	if err := repo.StartApproval(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	for _, want := range []bool{false, true} {
		if want {
			if err := repo.CompleteApproval(context.Background(), workflows.CompleteApprovalActivityInput{ApprovalID: start.ApprovalID, TenantID: tenant, ContractID: item.ID, Status: approval.StatusRejected}); err != nil {
				t.Fatal(err)
			}
		}
		meta, err := repo.GetApprovalMeta(context.Background(), tenant, start.ApprovalID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err = json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if value, exists := fields["completion_applied"]; !exists || value != want {
			t.Fatalf("完成事实未通过真实元数据暴露: got=%s want=%v", raw, want)
		}
	}
}
