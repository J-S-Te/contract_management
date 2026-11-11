package workflows

import (
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/workflow"
)

// StatusChangeWorkflow 驱动已授权的合同状态审批并保持旧输入初始化兼容。
// 参数：ctx为工作流上下文，input为冻结的审批身份与业务申请。
// 返回值：最终运行状态；初始化或关键持久化失败返回工作流错误。
func StatusChangeWorkflow(ctx workflow.Context, input StatusChangeInput) (ApprovalState, error) {
	if err := validateStatusChange(input); err != nil {
		return ApprovalState{}, err
	}
	if input.Timeout <= 0 {
		input.Timeout = 72 * time.Hour
	}
	now := workflow.Now(ctx)
	nodeName, pendingTitle := "管理员审批", "合同状态变更待审批"
	if input.PricingRevision {
		nodeName, pendingTitle = "合同定价修订审批", "合同定价修订待审批"
	}
	node := approval.Node{ID: "admin", Name: nodeName, RoleCode: "admin", AssigneeIDs: unique(input.AdminUserIDs), Countersign: approval.CountersignAny, Timeout: input.Timeout}
	state := ApprovalState{ApprovalID: input.ApprovalID, Kind: approval.KindStatusChange, Status: approval.StatusRunning, ContractID: input.ContractID, ApplicantUserID: input.ApplicantUserID, ApplicantDisplayName: input.ApplicantDisplayName, Nodes: initializeNodes([]approval.Node{node}), StartedAt: now, UpdatedAt: now}
	state.Nodes[0].Status, state.Nodes[0].StartedAt = approval.NodeActive, now
	if err := workflow.SetQueryHandler(ctx, StateQueryName, func() (ApprovalState, error) { return state, nil }); err != nil {
		return state, err
	}

	info := workflow.GetInfo(ctx)
	actx, cctx := activityContext(ctx), criticalActivityContext(ctx)
	start := StartApprovalActivityInput{RequireExisting: input.RequireExisting, ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, ExpectedVersion: input.ContractVersion, ApplicantUserID: input.ApplicantUserID, ApplicantDisplayName: input.ApplicantDisplayName, Kind: approval.KindStatusChange, FromStatus: input.FromStatus, TargetStatus: input.TargetStatus, Reason: input.Reason, WorkflowID: info.WorkflowExecution.ID, RunID: info.WorkflowExecution.RunID, Nodes: []approval.Node{node}, Audit: input.Audit}
	if err := executeLicensedActivity(ctx, cctx, ActivityStartApproval, start, nil); err != nil {
		return state, err
	}
	if err := notifyCurrentNode(ctx, actx, input.TenantID, state, "pending_approval", pendingTitle, input.Reason); err != nil {
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
			complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, Status: state.Status, Reason: state.FailureReason, WorkflowID: info.WorkflowExecution.ID, Audit: input.Audit}
			if err := executeLicensedActivity(ctx, cctx, ActivityCompleteApproval, complete, nil); err != nil {
				return state, err
			}
			if err := notifyRecipients(ctx, actx, input.TenantID, state, "rejected", "状态变更审批已超时", state.FailureReason, "expired", []string{input.ApplicantUserID}); err != nil {
				return state, err
			}
			break
		}
		if !gotCommand || validateCommonCommand(command) != nil ||
			contains(state.ProcessedCommandIDs, command.CommandID) || validateCommandOrigin(state, command) != nil {
			continue
		}
		if command.Action == ActionTransfer {
			// 历史状态审批曾忽略转派；仅新入口明确绑定来源的命令可产生转派活动。
			// 避免回放旧信号时插入历史中不存在的RecordCommand活动。
			if command.OriginNodeID == "" || command.OriginNodeRevision == nil {
				continue
			}
			changed, _, err := applyContractCommand(&state, command)
			if err != nil || !changed {
				continue
			}
			state.UpdatedAt = workflow.Now(ctx)
			if command.OriginNodeID != "" {
				state.ProcessedCommandIDs = append(state.ProcessedCommandIDs, command.CommandID)
			}
			record := RecordCommandActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, NodeID: node.ID, Command: command, State: state}
			if err := executeLicensedActivity(ctx, cctx, ActivityRecordCommand, record, nil); err != nil {
				return state, err
			}
			if err := notifyAssignedUsers(ctx, actx, input.TenantID, state, command, command.TargetUserIDs); err != nil {
				return state, err
			}
			continue
		}
		if command.Action == ActionUrge || command.Action == ActionComment {
			if command.OriginNodeID != "" {
				state.ProcessedCommandIDs = append(state.ProcessedCommandIDs, command.CommandID)
			}
			record := RecordCommandActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, NodeID: node.ID, Command: command, State: state}
			if err := executeLicensedActivity(ctx, cctx, ActivityRecordCommand, record, nil); err != nil {
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
		// BIZ-CONTRACT-03 分离职责：申请人不得批准自己发起的状态变更审批，
		// 即使其同时是当前节点审批人。application 层已做预检，这里是防御性
		// 第二道：被忽略的命令不改变状态，也不写入审批记录。
		case contains(state.Nodes[0].Node.AssigneeIDs, command.ActorUserID) && command.Action == ActionApprove && command.ActorUserID != input.ApplicantUserID:
			state.Status, state.Nodes[0].Status = approval.StatusApproved, approval.NodeApproved
		case contains(state.Nodes[0].Node.AssigneeIDs, command.ActorUserID) && command.Action == ActionReject:
			state.Status, state.Nodes[0].Status, state.FailureReason = approval.StatusRejected, approval.NodeRejected, command.Comment
		default:
			continue
		}
		if command.OriginNodeID != "" {
			state.ProcessedCommandIDs = append(state.ProcessedCommandIDs, command.CommandID)
		}
		state.CompletedAt, state.UpdatedAt = workflow.Now(ctx), workflow.Now(ctx)
		record := RecordCommandActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, NodeID: node.ID, Command: command, State: state}
		if err := executeLicensedActivity(ctx, cctx, ActivityRecordCommand, record, nil); err != nil {
			return state, err
		}
		target := input.FromStatus
		if state.Status == approval.StatusApproved {
			target = input.TargetStatus
		}
		complete := CompleteApprovalActivityInput{ApprovalID: input.ApprovalID, TenantID: input.TenantID, ContractID: input.ContractID, ActorUserID: command.ActorUserID, Status: state.Status, Reason: command.Comment, WorkflowID: info.WorkflowExecution.ID, TargetStatus: target, Audit: input.Audit}
		if err := executeLicensedActivity(ctx, cctx, ActivityCompleteApproval, complete, nil); err != nil {
			return state, err
		}
		typ, title := "rejected", "状态变更申请已拒绝"
		if state.Status == approval.StatusApproved {
			typ, title = "status_change", "合同状态已变更"
			if input.PricingRevision {
				title = "合同定价修订已批准"
			}
		} else if state.Status == approval.StatusWithdrawn {
			typ, title = "status_change", "状态变更申请已撤回"
		}
		if err := notifyRecipients(ctx, actx, input.TenantID, state, typ, title, command.Comment, string(state.Status), []string{input.ApplicantUserID}); err != nil {
			return state, err
		}
	}
	return state, nil
}

// validateStatusChange 校验普通状态变更或同状态定价修订审批输入。
// 参数：input为已受理的可信工作流输入。
// 返回值：合法返回空，非法状态或缺少业务原因返回错误。
func validateStatusChange(input StatusChangeInput) error {
	if input.ApprovalID == "" || input.ContractID == "" || input.ApplicantUserID == "" || len(input.AdminUserIDs) == 0 {
		return fmt.Errorf("invalid status change input")
	}
	if input.PricingRevision {
		if input.RequireExisting && input.FromStatus == input.TargetStatus && input.FromStatus.ApprovalPassed() && input.Reason != "" {
			return nil
		}
		return fmt.Errorf("invalid pricing revision")
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
