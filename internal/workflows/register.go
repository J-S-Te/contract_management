package workflows

import (
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func Register(w worker.Worker, activities *Activities) {
	w.RegisterWorkflowWithOptions(ContractApprovalWorkflow, workflow.RegisterOptions{Name: ContractApprovalWorkflowName})
	w.RegisterWorkflowWithOptions(StatusChangeWorkflow, workflow.RegisterOptions{Name: StatusChangeWorkflowName})
	w.RegisterWorkflowWithOptions(ExpiredContractArchiveWorkflow, workflow.RegisterOptions{Name: ExpiredArchiveWorkflowName})
	w.RegisterActivity(activities)
}
