package mysql

import (
	"context"
	"fmt"

	"github.com/j-s-te/contract-management/internal/workflows"
)

// 审批名称优先使用提交快照，旧审批缺少名称时才回退当前合同；查询必须限定 c、s 的租户。
const approvalContractTitleSQL = "COALESCE(" +
	"NULLIF(JSON_UNQUOTE(JSON_EXTRACT(s.contract_json, '$.title')), ''), c.title, '')"

// approvalNotificationContent 为待审批及催办通知补充审批时的合同名称。
// 参数：ctx为执行上下文，in为通知所属租户、合同、审批及原始正文。
// 返回值：包含合同名称的正文；查询失败时返回错误，其他通知保留原正文。
func (r *Repository) approvalNotificationContent(
	ctx context.Context, in workflows.NotifyActivityInput,
) (string, error) {
	switch in.Type {
	case "pending_approval", "approval_reminder", "approval_assigned":
	default:
		return in.Content, nil
	}
	var item struct{ Title string }
	err := r.db.WithContext(ctx).Table("con_contract AS c").
		Select(approvalContractTitleSQL+" AS title").
		Joins("LEFT JOIN con_approval_snapshot AS s ON s.approval_id = ? "+
			"AND s.tenant_id = c.tenant_id AND s.contract_id = c.id", in.ApprovalID).
		Where("c.tenant_id = ? AND c.id = ?", in.TenantID, in.ContractID).
		Where("c.deleted_at IS NULL").
		Take(&item).Error
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("合同名称：%s\n%s", item.Title, in.Content), nil
}
