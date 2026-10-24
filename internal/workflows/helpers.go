package workflows

import (
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const commercialLicenseErrorType = "COMMERCIAL_LICENSE_RESTRICTED"

// 撤销或过期许可证会持久暂停待处理业务。若将
// 拒绝视为普通活动失败，会耗尽 MaximumAttempts 并丢失
// 工作流。其他失败保留既有有界重试策略。
func executeLicensedActivity(ctx, actx workflow.Context, name string, input, output any) error {
	for {
		err := workflow.ExecuteActivity(actx, name, input).Get(ctx, output)
		var licenseErr *temporal.ApplicationError
		if !errors.As(err, &licenseErr) || licenseErr.Type() != commercialLicenseErrorType {
			return err
		}
		if err := workflow.Sleep(ctx, 30*time.Second); err != nil {
			return err
		}
	}
}

func activityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second, MaximumAttempts: 8},
	})
}

// criticalActivityContext 用于状态流转本身。数据库/网络类故障不应在有限重试后把
// 工作流打成失败、让合同永久停在“待审批”；非可重试的业务冲突由 Activity 层
// 包装为 NonRetryable，仍会立即失败。通知等旁路 Activity 继续使用有限重试。
func criticalActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute,
			MaximumAttempts: 0,
		},
	})
}

func initializeNodes(nodes []approval.Node) []RuntimeNode {
	result := make([]RuntimeNode, len(nodes))
	for i, node := range nodes {
		if node.Countersign == "" {
			node.Countersign = approval.CountersignAll
		}
		result[i] = RuntimeNode{Node: node, Status: approval.NodePending, ApprovedBy: map[string]bool{}}
	}
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func unique(values []string) []string {
	seen, result := map[string]struct{}{}, make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validateCommonCommand(command ApprovalCommand) error {
	if command.CommandID == "" || command.ActorUserID == "" {
		return fmt.Errorf("command_id and actor_user_id are required")
	}
	switch command.Action {
	case ActionReject, ActionAddSign, ActionTransfer, ActionReturn, ActionWithdraw:
		if command.Comment == "" {
			return fmt.Errorf("comment is required for %s", command.Action)
		}
	}
	return nil
}

func notifyCurrentNode(ctx, actx workflow.Context, tenantID string, state ApprovalState, typ, title, content string) error {
	if state.CurrentNodeIndex >= len(state.Nodes) {
		return nil
	}
	node := state.Nodes[state.CurrentNodeIndex]
	in := NotifyActivityInput{TenantID: tenantID, ApprovalID: state.ApprovalID, ContractID: state.ContractID, Type: typ, Recipients: node.Node.AssigneeIDs, Title: title, Content: content, DedupeKey: fmt.Sprintf("%s:%s:%s:%d", state.ApprovalID, node.Node.ID, typ, workflow.Now(ctx).Unix()/3600)}
	return notifyBestEffort(ctx, actx, in)
}

// notifyBestEffort 把通知视为旁路：通知写入失败不能阻断已经完成或正在进行的审批
// 状态流转，但必须留下工作流日志供追踪；事务通知 Outbox 仍由独立投递器重试。
func notifyBestEffort(ctx, actx workflow.Context, in NotifyActivityInput) error {
	if err := executeLicensedActivity(ctx, actx, ActivityCreateNotification, in, nil); err != nil {
		workflow.GetLogger(ctx).Error("contract notification activity failed; approval state continues",
			"approval_id", in.ApprovalID, "contract_id", in.ContractID, "type", in.Type, "error", err)
	}
	return nil
}

func notifyRecipients(ctx, actx workflow.Context, tenantID string, state ApprovalState, typ, title, content, keySuffix string, recipients []string) error {
	in := NotifyActivityInput{TenantID: tenantID, ApprovalID: state.ApprovalID, ContractID: state.ContractID, Type: typ, Recipients: unique(recipients), Title: title, Content: content, DedupeKey: state.ApprovalID + ":" + keySuffix}
	return notifyBestEffort(ctx, actx, in)
}

// notifyAssignedUsers 仅通知当前命令新增的用户，有意
// 独立于 notifyCurrentNode：转交或加签改变同一节点时，
// 既有受理人不应收到重复消息。
func notifyAssignedUsers(ctx, actx workflow.Context, tenantID string, state ApprovalState, command ApprovalCommand, recipients []string) error {
	if len(unique(recipients)) == 0 {
		return nil
	}
	return notifyRecipients(ctx, actx, tenantID, state, "approval_assigned", "您已被指派合同审批待办", "您已被指派参与当前合同审批，请及时处理", "assigned:"+command.CommandID, recipients)
}

func notifyRoles(ctx, actx workflow.Context, tenantID string, state ApprovalState, typ, title, content, keySuffix string, roles []string) error {
	in := NotifyActivityInput{TenantID: tenantID, ApprovalID: state.ApprovalID, ContractID: state.ContractID, Type: typ, RoleRecipients: unique(roles), Title: title, Content: content, DedupeKey: state.ApprovalID + ":" + keySuffix}
	return notifyBestEffort(ctx, actx, in)
}
