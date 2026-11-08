package mysql

import (
	"context"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/workflows"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AcceptStatusChange 在合同行锁保护下原子受理审批并执行有界启动回调。
// 参数：ctx为请求上下文，in为冻结申请，start仅启动工作流并返回RunID。
// 返回值：成功后审批可读取；任何初始化、启动或提交错误均回滚源事务。
func (r *Repository) AcceptStatusChange(ctx context.Context, in workflows.StartApprovalActivityInput, start func(context.Context) (string, error)) error {
	if in.Kind != approval.KindStatusChange || in.RequireExisting || in.RunID != "" || start == nil {
		return apperrors.ErrStateConflict
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 先拿合同行锁再建立一致性读快照，避免并发等待后仍漏读上一事务的审批。
		var current contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").
			Where("tenant_id=? AND id=?", in.TenantID, in.ContractID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		var active int64
		if err := tx.Model(&approvalInstanceRecord{}).
			Where("tenant_id=? AND contract_id=? AND kind=? AND (status=? OR active_status_change_key IS NOT NULL)",
				in.TenantID, in.ContractID, approval.KindStatusChange, approval.StatusRunning).
			Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return apperrors.ErrStateConflict
		}
		if err := startApprovalInTx(tx, in); err != nil {
			return err
		}
		runID, err := start(ctx)
		if err != nil {
			return err
		}
		if runID == "" {
			return apperrors.ErrStateConflict
		}
		result := tx.Model(&approvalInstanceRecord{}).Where("tenant_id=? AND id=? AND temporal_workflow_id=? AND temporal_run_id=''", in.TenantID, in.ApprovalID, in.WorkflowID).Update("temporal_run_id", runID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrStateConflict
		}
		return nil
	})
}

// initializeAcceptedStatusChange 锁定已受理记录并幂等绑定真实工作流执行。
// 参数：tx为Worker初始化事务，in为RequireExisting的新工作流输入。
// 返回值：未受理返回未找到；身份或执行漂移返回冲突；不自行新建审批。
func initializeAcceptedStatusChange(tx *gorm.DB, in workflows.StartApprovalActivityInput) error {
	var row approvalInstanceRecord
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", in.TenantID, in.ApprovalID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return apperrors.ErrNotFound
	}
	if err != nil {
		return err
	}
	if row.Kind != string(approval.KindStatusChange) || row.ContractID != in.ContractID || row.TemporalWorkflowID != in.WorkflowID ||
		row.ApplicantUserID != in.ApplicantUserID || row.FromStatus != string(in.FromStatus) || row.TargetStatus != string(in.TargetStatus) ||
		row.Status != string(approval.StatusRunning) || row.CompletionApplied || in.RunID == "" || (row.TemporalRunID != "" && row.TemporalRunID != in.RunID) {
		return apperrors.ErrStateConflict
	}
	if row.TemporalRunID == in.RunID {
		return nil
	}
	result := tx.Model(&approvalInstanceRecord{}).
		Where("tenant_id=? AND id=? AND temporal_run_id=''", in.TenantID, in.ApprovalID).
		Updates(map[string]any{"temporal_run_id": in.RunID, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrStateConflict
	}
	return nil
}
