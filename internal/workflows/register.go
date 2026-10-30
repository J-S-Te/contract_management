package workflows

import (
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// Register 注册普通审批和独立历史确认等现有Temporal工作流与活动。
// 参数：w为目标Worker，activities为绑定业务依赖的活动集合。
// 返回值：无。
func Register(w worker.Worker, activities *Activities) {
	w.RegisterWorkflowWithOptions(ContractApprovalWorkflow, workflow.RegisterOptions{Name: ContractApprovalWorkflowName})
	w.RegisterWorkflowWithOptions(StatusChangeWorkflow, workflow.RegisterOptions{Name: StatusChangeWorkflowName})
	w.RegisterWorkflowWithOptions(ExpiredContractArchiveWorkflow, workflow.RegisterOptions{Name: ExpiredArchiveWorkflowName})
	w.RegisterWorkflowWithOptions(ImportConfirmationWorkflow, workflow.RegisterOptions{Name: ImportConfirmationWorkflowName})
	w.RegisterActivity(activities)
}
