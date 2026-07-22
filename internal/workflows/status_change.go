package workflows

import (
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/workflow"
)

// StatusChangeWorkflow implements APP-002 for critical lifecycle targets.
func StatusChangeWorkflow(ctx workflow.Context, input StatusChangeInput) (ApprovalState, error) {
	if err := validateStatusChange(input); err != nil {
		return ApprovalState{}, err
	}
	if input.Timeout <= 0 {
		input.Timeout = 72 * time.Hour
	}
	now := workflow.Now(ctx)
	node := approval.Node{ID: "administrator", Name: "管理员审批", RoleCode: "administrator", AssigneeIDs: unique(input.AdminUserIDs), Countersign: approval.CountersignAny, Timeout: input.Timeout}
	state := ApprovalState{ApprovalID: input.ApprovalID, Kind: approval.KindStatusChange, Status: approval.StatusRunning, ContractID: input.ContractID, ApplicantUserID: input.ApplicantUserID, Nodes: initializeNodes([]approval.Node{node}), StartedAt: now, UpdatedAt: now}
	state.Nodes[0].Status, state.Nodes[0].StartedAt = approval.NodeActive, now
	if err := workflow.SetQueryHandler(ctx, StateQueryName, func() (ApprovalState, error) { return state, nil }); err != nil {
		return state, err
	}

	info, actx := workflow.GetInfo(ctx), activityContext(ctx)
	start := StartApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, ExpectedVersion: input.ContractVersion, ApplicantUserID: input.ApplicantUserID, Kind: approval.KindStatusChange, FromStatus: input.FromStatus, TargetStatus: input.TargetStatus, Reason: input.Reason, WorkflowID: info.WorkflowExecution.ID, RunID: info.WorkflowExecution.RunID, Nodes: []approval.Node{node}}
	if err := workflow.ExecuteActivity(actx, ActivityStartApproval, start).Get(ctx, nil); err != nil {
		return state, err
	}
	if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "pending_approval", "合同状态变更待审批", input.Reason); err != nil {
		return state, err
	}

	commandCh, timer := workflow.GetSignalChannel(ctx, CommandSignalName), workflow.NewTimer(ctx, input.Timeout)
	for state.Status == approval.StatusRunning {
		selector := workflow.NewSelector(ctx)
		var command ApprovalCommand
		gotCommand, expired := false, false
		selector.AddReceive(commandCh, func(ch workflow.ReceiveChannel, _ bool) { ch.Receive(ctx, &command); gotCommand = true })
		selector.AddFuture(timer, func(workflow.Future) { expired = true })
		selector.Select(ctx)
		if expired {
			now = workflow.Now(ctx)
			state.Status, state.FailureReason, state.CompletedAt, state.UpdatedAt = approval.StatusExpired, "status change approval timed out", now, now
			complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, Status: state.Status, Reason: state.FailureReason, WorkflowID: info.WorkflowExecution.ID}
			if err := workflow.ExecuteActivity(actx, ActivityCompleteApproval, complete).Get(ctx, nil); err != nil {
				return state, err
			}
			if err := notifyRecipients(ctx, actx, input.TenantID, state, "rejected", "状态变更审批已超时", state.FailureReason, "expired", []string{input.ApplicantUserID}); err != nil {
				return state, err
			}
			break
		}
		if !gotCommand || validateCommonCommand(command) != nil {
			continue
		}
		if command.Action == ActionUrge || command.Action == ActionComment {
			record := RecordCommandActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, NodeID: node.ID, Command: command, State: state}
			if err := workflow.ExecuteActivity(actx, ActivityRecordCommand, record).Get(ctx, nil); err != nil {
				return state, err
			}
			if command.Action == ActionUrge {
				if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "approval_reminder", "合同状态变更催办", command.Comment); err != nil {
					return state, err
				}
			}
			continue
		}
		switch {
		case command.Action == ActionWithdraw && command.ActorUserID == input.ApplicantUserID:
			state.Status = approval.StatusWithdrawn
		case contains(node.AssigneeIDs, command.ActorUserID) && command.Action == ActionApprove:
			state.Status, state.Nodes[0].Status = approval.StatusApproved, approval.NodeApproved
		case contains(node.AssigneeIDs, command.ActorUserID) && command.Action == ActionReject:
			state.Status, state.Nodes[0].Status, state.FailureReason = approval.StatusRejected, approval.NodeRejected, command.Comment
		default:
			continue
		}
		state.CompletedAt, state.UpdatedAt = workflow.Now(ctx), workflow.Now(ctx)
		record := RecordCommandActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, NodeID: node.ID, Command: command, State: state}
		if err := workflow.ExecuteActivity(actx, ActivityRecordCommand, record).Get(ctx, nil); err != nil {
			return state, err
		}
		target := input.FromStatus
		if state.Status == approval.StatusApproved {
			target = input.TargetStatus
		}
		complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, ActorUserID: command.ActorUserID, Status: state.Status, Reason: command.Comment, WorkflowID: info.WorkflowExecution.ID, TargetStatus: target}
		if err := workflow.ExecuteActivity(actx, ActivityCompleteApproval, complete).Get(ctx, nil); err != nil {
			return state, err
		}
		typ, title := "rejected", "状态变更申请已拒绝"
		if state.Status == approval.StatusApproved {
			typ, title = "status_change", "合同状态已变更"
		} else if state.Status == approval.StatusWithdrawn {
			typ, title = "status_change", "状态变更申请已撤回"
		}
		if err := notifyRecipients(ctx, actx, input.TenantID, state, typ, title, command.Comment, string(state.Status), []string{input.ApplicantUserID}); err != nil {
			return state, err
		}
	}
	return state, nil
}

func validateStatusChange(input StatusChangeInput) error {
	if input.ApprovalID == "" || input.ContractID == "" || input.ApplicantUserID == "" || len(input.AdminUserIDs) == 0 {
		return fmt.Errorf("invalid status change input")
	}
	if err := contract.ValidateTransition(input.FromStatus, input.TargetStatus); err != nil {
		return err
	}
	if !input.TargetStatus.RequiresApproval() {
		return fmt.Errorf("target status %s does not require approval", input.TargetStatus)
	}
	if input.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}
