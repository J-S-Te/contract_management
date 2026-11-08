package application

import (
	"context"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	temporalmocks "go.temporal.io/sdk/mocks"
)

// TestChangeStatusNewRoundUsesDistinctWorkflow 验证同版本新申请拥有独立审批身份。
// 参数：t为测试上下文，模拟前轮未修改合同版本的重新申请。
// 返回值：无；新轮次复用旧Workflow或放宽重复执行策略时失败。
func TestChangeStatusNewRoundUsesDistinctWorkflow(t *testing.T) {
	repo := &recordingRepository{contract: statusAuthorizationContract("tenant-1", "sales-1", contract.StatusInProgress)}
	temporal := temporalmocks.NewClient(t)
	run := temporalmocks.NewWorkflowRun(t)
	run.On("GetID").Return("workflow-result")
	run.On("GetRunID").Return("run-result")
	temporal.On("ExecuteWorkflow", mock.Anything, mock.Anything, workflows.StatusChangeWorkflowName, mock.Anything).Return(run, nil).Twice()
	service := &Service{Repo: repo, Personnel: &statusAuthorizationPersonnel{}, Temporal: temporal}
	for i := 0; i < 2; i++ {
		if _, err := service.ChangeStatus(context.Background(), statusAuthorizationActor("sales"), "contract-1", 3, contract.StatusPendingPay, "重新提交申请"); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]bool{}
	for _, call := range temporal.Calls {
		options := call.Arguments.Get(1).(client.StartWorkflowOptions)
		input := call.Arguments.Get(3).(workflows.StatusChangeInput)
		if ids[options.ID] {
			t.Fatalf("同版本的新申请复用了已关闭Workflow: %s", options.ID)
		}
		ids[options.ID] = true
		if !strings.Contains(options.ID, input.ApprovalID) || options.WorkflowIDReusePolicy != enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
			t.Fatalf("工作流身份未绑定审批ID或同申请去重被放宽: options=%+v input=%+v", options, input)
		}
	}
}
