package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/j-s-te/contract-management/internal/workflows"
)

// approvalCommandID 按租户、审批及操作语义派生稳定命令身份。
// 参数：tenantID和approvalID为隔离边界，command为已绑定主体和原任务的命令。
// 返回值：与数据库命令列兼容的26位标识。
func approvalCommandID(tenantID, approvalID string, command workflows.ApprovalCommand) string {
	payload, _ := json.Marshal(struct {
		Tenant, Approval, Actor, Origin, Action, Comment, Target string
		Revision                                                 *uint64
		Users                                                    []string
		Countersign                                              string
	}{
		Tenant: tenantID, Approval: approvalID, Actor: command.ActorUserID,
		Origin: command.OriginNodeID, Action: string(command.Action), Comment: command.Comment,
		Target: command.TargetNodeID, Revision: command.OriginNodeRevision,
		Users: command.TargetUserIDs, Countersign: string(command.Countersign)})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:13])
}

// approvalTransferPersisted 核对转派动作与任务投影已在同一持久化事务中提交。
// 参数：ctx为上下文，其余参数限定租户、审批、节点、原处理人与命令身份。
// 返回值：仅精确匹配的持久化转派返回true，读取失败返回错误。
func (s *Service) approvalTransferPersisted(ctx context.Context, tenantID, approvalID, nodeID, actorID, commandID string) (bool, error) {
	actions, err := s.Repo.ListApprovalActions(ctx, tenantID, approvalID)
	if err != nil {
		return false, err
	}
	for _, action := range actions {
		if action.CommandID == commandID && action.NodeID == nodeID && action.ActorUserID == actorID && action.Action == string(workflows.ActionTransfer) {
			return true, nil
		}
	}
	return false, nil
}
