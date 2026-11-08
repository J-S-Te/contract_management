package application

import (
	"context"
	"github.com/j-s-te/contract-management/internal/workflows"
)

// StatusChangeApprovalAcceptor 将审批持久化与有界工作流启动放入同一源事务。
// 启动失败必须回滚审批、任务和快照；回调不得等待Worker执行完成。
type StatusChangeApprovalAcceptor interface {
	// AcceptStatusChange 独占受理状态变更申请并在提交前启动工作流。
	// 参数：ctx为请求上下文，in为审批初始化内容，start仅启动并返回执行ID。
	// 返回值：在途冲突、启动或提交失败；成功后审批身份已持久化。
	AcceptStatusChange(context.Context, workflows.StartApprovalActivityInput, func(context.Context) (string, error)) error
}
