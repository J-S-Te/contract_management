package workflows

import (
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/testsuite"
)

// TestPricingRevisionUsesIndependentExistingWorkflow 验证同状态定价修订沿原审批且不能自审。
// 参数：t为测试上下文；Temporal使用原生确定性测试环境。
// 返回值：无；自审不得产生决定，独立审批才可终决。
func TestPricingRevisionUsesIndependentExistingWorkflow(t *testing.T) {
	store := &memoryStore{}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Store: store})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "self", Action: ActionApprove, ActorUserID: "applicant", OccurredAt: time.Now().UTC()})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(CommandSignalName, ApprovalCommand{CommandID: "independent", Action: ActionApprove, ActorUserID: "reviewer", OccurredAt: time.Now().UTC()})
	}, 2*time.Minute)
	env.ExecuteWorkflow(StatusChangeWorkflow, StatusChangeInput{PricingRevision: true, RequireExisting: true, ApprovalID: "revision", TenantID: "tenant", ContractID: "contract", ContractVersion: 3, ApplicantUserID: "applicant", FromStatus: contract.StatusActive, TargetStatus: contract.StatusActive, Reason: "独立重选", AdminUserIDs: []string{"applicant", "reviewer"}, Timeout: 72 * time.Hour})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(store.completed) != 1 || store.completed[0].ActorUserID != "reviewer" || store.completed[0].Status != approval.StatusApproved || store.completed[0].TargetStatus != contract.StatusActive {
		t.Fatalf("终决不符：%+v", store.completed)
	}
	for _, command := range store.commands {
		if command.CommandID == "self" {
			t.Fatal("自审命令被写入")
		}
	}
}
