package workflows

import "fmt"

// validateCommandOrigin 拒绝跨节点、退回前或转派前的旧任务命令并兼容历史输入。
// 参数：state为消费时运行状态，command为带可选来源身份的命令。
// 返回值：身份一致或历史命令返回nil，旧任务返回错误。
func validateCommandOrigin(state ApprovalState, command ApprovalCommand) error {
	if command.OriginNodeID == "" && command.OriginNodeRevision == nil {
		return nil
	}
	if state.CurrentNodeIndex < 0 || state.CurrentNodeIndex >= len(state.Nodes) {
		return fmt.Errorf("no active approval node")
	}
	node := state.Nodes[state.CurrentNodeIndex]
	if command.OriginNodeID != node.Node.ID || command.OriginNodeRevision == nil ||
		*command.OriginNodeRevision != node.Revision {
		return fmt.Errorf("approval task has changed")
	}
	return nil
}
