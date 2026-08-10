package workflows

import (
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func activityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second, MaximumAttempts: 8},
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
	return workflow.ExecuteActivity(actx, ActivityCreateNotification, in).Get(ctx, nil)
}

func notifyRecipients(ctx, actx workflow.Context, tenantID string, state ApprovalState, typ, title, content, keySuffix string, recipients []string) error {
	in := NotifyActivityInput{TenantID: tenantID, ApprovalID: state.ApprovalID, ContractID: state.ContractID, Type: typ, Recipients: unique(recipients), Title: title, Content: content, DedupeKey: state.ApprovalID + ":" + keySuffix}
	return workflow.ExecuteActivity(actx, ActivityCreateNotification, in).Get(ctx, nil)
}

func notifyRoles(ctx, actx workflow.Context, tenantID string, state ApprovalState, typ, title, content, keySuffix string, roles []string) error {
	in := NotifyActivityInput{TenantID: tenantID, ApprovalID: state.ApprovalID, ContractID: state.ContractID, Type: typ, RoleRecipients: unique(roles), Title: title, Content: content, DedupeKey: state.ApprovalID + ":" + keySuffix}
	return workflow.ExecuteActivity(actx, ActivityCreateNotification, in).Get(ctx, nil)
}
