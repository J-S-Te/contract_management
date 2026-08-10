package workflows

import (
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/workflow"
)

// ContractApprovalWorkflow implements APP-001 and APP-005. Approval nodes are
// snapshotted in input, so later rule edits only affect newly started runs.
func ContractApprovalWorkflow(ctx workflow.Context, input ContractApprovalInput) (ApprovalState, error) {
	if input.ApprovalID == "" || input.ContractID == "" || input.ApplicantUserID == "" || len(input.Nodes) == 0 {
		return ApprovalState{}, fmt.Errorf("invalid contract approval input")
	}
	if input.DefaultNodeTimeout <= 0 {
		input.DefaultNodeTimeout = 72 * time.Hour
	}
	if input.ReminderInterval <= 0 {
		input.ReminderInterval = 24 * time.Hour
	}

	now := workflow.Now(ctx)
	state := ApprovalState{ApprovalID: input.ApprovalID, Kind: approval.KindContract, Status: approval.StatusRunning, ContractID: input.ContractID, ApplicantUserID: input.ApplicantUserID, ApplicantDisplayName: input.ApplicantDisplayName, Nodes: initializeNodes(input.Nodes), StartedAt: now, UpdatedAt: now}
	if err := workflow.SetQueryHandler(ctx, StateQueryName, func() (ApprovalState, error) { return state, nil }); err != nil {
		return state, err
	}

	info, actx := workflow.GetInfo(ctx), activityContext(ctx)
	start := StartApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, ExpectedVersion: input.ContractVersion, ApplicantUserID: input.ApplicantUserID, ApplicantDisplayName: input.ApplicantDisplayName, Kind: approval.KindContract, FromStatus: contract.StatusDraft, TargetStatus: contract.StatusPending, RuleID: input.RuleID, RuleVersion: input.RuleVersion, ContentHash: input.ContentHash, WorkflowID: info.WorkflowExecution.ID, RunID: info.WorkflowExecution.RunID, Nodes: input.Nodes}
	if err := workflow.ExecuteActivity(actx, ActivityStartApproval, start).Get(ctx, nil); err != nil {
		return state, err
	}

	commandCh := workflow.GetSignalChannel(ctx, CommandSignalName)
	for state.Status == approval.StatusRunning {
		if state.CurrentNodeIndex >= len(state.Nodes) {
			now = workflow.Now(ctx)
			state.Status, state.CompletedAt, state.UpdatedAt = approval.StatusApproved, now, now
			complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, Status: state.Status, Reason: "all approval nodes passed", WorkflowID: info.WorkflowExecution.ID, TargetStatus: contract.StatusActive}
			if err := workflow.ExecuteActivity(actx, ActivityCompleteApproval, complete).Get(ctx, nil); err != nil {
				return state, err
			}
			if err := notifyRecipients(ctx, actx, input.TenantID, state, "approved", "合同审批已通过", "合同已批准并生效", "approved", []string{input.ApplicantUserID}); err != nil {
				return state, err
			}
			if workflow.GetVersion(ctx, "contract-specialist-signing-notification", workflow.DefaultVersion, 1) != workflow.DefaultVersion {
				if err := notifyRoles(ctx, actx, input.TenantID, state, "signing_pending", "合同审批已通过，待办理签署", "有一份已生效合同等待登记寄出和回传跟踪", "contract-specialist-signing", []string{"contract_specialist"}); err != nil {
					return state, err
				}
			}
			break
		}

		node := &state.Nodes[state.CurrentNodeIndex]
		if node.Status != approval.NodeActive {
			node.Status, node.StartedAt = approval.NodeActive, workflow.Now(ctx)
			state.UpdatedAt = node.StartedAt
			if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "pending_approval", "合同审批待处理", "您有一项新的合同审批待办"); err != nil {
				return state, err
			}
		}
		timeout := node.Node.Timeout
		if timeout <= 0 {
			timeout = input.DefaultNodeTimeout
		}
		deadline := node.StartedAt.Add(timeout)
		nextReminder := workflow.Now(ctx).Add(input.ReminderInterval)

		for state.Status == approval.StatusRunning && node.Status == approval.NodeActive {
			now = workflow.Now(ctx)
			wakeAt := deadline
			if nextReminder.Before(wakeAt) {
				wakeAt = nextReminder
			}
			wait := wakeAt.Sub(now)
			if wait < 0 {
				wait = 0
			}
			timerCtx, cancelTimer := workflow.WithCancel(ctx)
			timer := workflow.NewTimer(timerCtx, wait)
			selector := workflow.NewSelector(ctx)
			var command ApprovalCommand
			gotCommand, timerFired := false, false
			selector.AddReceive(commandCh, func(ch workflow.ReceiveChannel, _ bool) { ch.Receive(ctx, &command); gotCommand = true })
			selector.AddFuture(timer, func(workflow.Future) { timerFired = true })
			selector.Select(ctx)
			if gotCommand {
				cancelTimer()
			}

			if timerFired {
				now = workflow.Now(ctx)
				if !now.Before(deadline) {
					state.Status, state.FailureReason, state.CompletedAt, state.UpdatedAt = approval.StatusExpired, "current approval node timed out", now, now
					complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, Status: state.Status, Reason: state.FailureReason, WorkflowID: info.WorkflowExecution.ID, TargetStatus: contract.StatusDraft}
					if err := workflow.ExecuteActivity(actx, ActivityCompleteApproval, complete).Get(ctx, nil); err != nil {
						return state, err
					}
					if err := notifyRecipients(ctx, actx, input.TenantID, state, "rejected", "合同审批已超时", state.FailureReason, "expired", []string{input.ApplicantUserID}); err != nil {
						return state, err
					}
					break
				}
				if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "approval_reminder", "合同审批即将超时", "请及时处理合同审批待办"); err != nil {
					return state, err
				}
				nextReminder = now.Add(input.ReminderInterval)
				continue
			}
			if !gotCommand || validateCommonCommand(command) != nil {
				continue
			}
			previousNodeIndex := state.CurrentNodeIndex
			changed, terminal, err := applyContractCommand(&state, command)
			if err != nil {
				continue
			}
			state.UpdatedAt = workflow.Now(ctx)
			advancedToNextNode := changed && !terminal && state.CurrentNodeIndex != previousNodeIndex && state.CurrentNodeIndex < len(state.Nodes)
			if advancedToNextNode {
				nextNode := &state.Nodes[state.CurrentNodeIndex]
				nextNode.Status, nextNode.StartedAt = approval.NodeActive, state.UpdatedAt
			}
			if changed {
				record := RecordCommandActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, NodeID: node.Node.ID, Command: command, State: state}
				if err := workflow.ExecuteActivity(actx, ActivityRecordCommand, record).Get(ctx, nil); err != nil {
					return state, err
				}
			}
			if advancedToNextNode {
				if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "pending_approval", "合同审批待处理", "您有一项新的合同审批待办"); err != nil {
					return state, err
				}
			}
			if command.Action == ActionUrge {
				if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "approval_reminder", "合同审批催办", command.Comment); err != nil {
					return state, err
				}
			}
			if terminal {
				state.CompletedAt = workflow.Now(ctx)
				complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, ActorUserID: command.ActorUserID, Status: state.Status, Reason: command.Comment, WorkflowID: info.WorkflowExecution.ID, TargetStatus: contract.StatusDraft}
				if err := workflow.ExecuteActivity(actx, ActivityCompleteApproval, complete).Get(ctx, nil); err != nil {
					return state, err
				}
				typ, title := "rejected", "合同审批已拒绝"
				if state.Status == approval.StatusWithdrawn {
					typ, title = "status_change", "合同审批已撤回"
				}
				if err := notifyRecipients(ctx, actx, input.TenantID, state, typ, title, command.Comment, string(state.Status), []string{input.ApplicantUserID}); err != nil {
					return state, err
				}
			}
		}
	}
	return state, nil
}

func applyContractCommand(state *ApprovalState, command ApprovalCommand) (changed, terminal bool, err error) {
	if command.Action == ActionWithdraw {
		if command.ActorUserID != state.ApplicantUserID {
			return false, false, fmt.Errorf("only applicant may withdraw")
		}
		state.Status, state.FailureReason = approval.StatusWithdrawn, command.Comment
		return true, true, nil
	}
	if command.Action == ActionUrge || command.Action == ActionComment {
		return true, false, nil
	}
	node := &state.Nodes[state.CurrentNodeIndex]
	if !contains(node.Node.AssigneeIDs, command.ActorUserID) {
		return false, false, fmt.Errorf("actor is not a current assignee")
	}
	switch command.Action {
	case ActionApprove:
		node.ApprovedBy[command.ActorUserID] = true
		passed := node.Node.Countersign == approval.CountersignAny
		if !passed {
			passed = true
			for _, id := range node.Node.AssigneeIDs {
				if !node.ApprovedBy[id] {
					passed = false
					break
				}
			}
		}
		if passed {
			node.Status, node.CompletedAt = approval.NodeApproved, command.OccurredAt
			state.CurrentNodeIndex++
		}
		return true, false, nil
	case ActionReject:
		node.Status, state.Status, state.FailureReason = approval.NodeRejected, approval.StatusRejected, command.Comment
		return true, true, nil
	case ActionAddSign:
		targets := unique(command.TargetUserIDs)
		if len(targets) == 0 {
			return false, false, fmt.Errorf("target users required")
		}
		if command.Countersign != approval.CountersignAll && command.Countersign != approval.CountersignAny {
			return false, false, fmt.Errorf("invalid countersign mode")
		}
		for _, target := range targets {
			if !contains(node.Node.AssigneeIDs, target) {
				node.Node.AssigneeIDs = append(node.Node.AssigneeIDs, target)
			}
		}
		node.Node.Countersign = command.Countersign
		return true, false, nil
	case ActionTransfer:
		targets := unique(command.TargetUserIDs)
		if len(targets) != 1 {
			return false, false, fmt.Errorf("one transfer target required")
		}
		for i, id := range node.Node.AssigneeIDs {
			if id == command.ActorUserID {
				node.Node.AssigneeIDs[i] = targets[0]
				break
			}
		}
		delete(node.ApprovedBy, command.ActorUserID)
		return true, false, nil
	case ActionReturn:
		target := -1
		for i := 0; i < state.CurrentNodeIndex; i++ {
			if state.Nodes[i].Node.ID == command.TargetNodeID && state.Nodes[i].Status == approval.NodeApproved {
				target = i
				break
			}
		}
		if target < 0 {
			return false, false, fmt.Errorf("target must be an approved historical node")
		}
		for i := target; i <= state.CurrentNodeIndex; i++ {
			state.Nodes[i].Status = approval.NodePending
			state.Nodes[i].ApprovedBy = map[string]bool{}
			state.Nodes[i].StartedAt = time.Time{}
			state.Nodes[i].CompletedAt = time.Time{}
		}
		state.CurrentNodeIndex = target
		return true, false, nil
	default:
		return false, false, fmt.Errorf("unsupported action %q", command.Action)
	}
}
