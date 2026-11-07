package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AutoConfirmHistoricalImportItem 把一条历史开立项自动确认为清单声明终态。
//
// 使用场景：CONTRACT_IMPORT_AUTO_APPROVE_HISTORICAL 能力开启且租户运行开关为真的
// 历史导入。CRM 客户/商机开立完成时合同行刚由 CommitImportDraft 写成 version=1、SourceFileStatus=READY、
// OriginKind=IMPORTED，正是 confirmHistoricalItem 要求的前置基线，因此这里直接复用
// 既有确认写入：更新 status=声明状态、version=2、分配系统号、写 IMPORT_CONFIRMED
// 审计并生成 PROJECT/FINANCE/REPORT 目标，而不是另写一套归档逻辑。
//
// 参数：ctx 为上下文，tenantID/batchID/itemID 定位批次与单项，roles 为合成确认台账
// 记录的审批角色（开启自动审批时来自 CONTRACT_IMPORT_CONFIRMATION_ROLES）。
// 返回值：幂等成功为 nil；批次/单项不存在、确认流程已被人工占用、清单或合同基线
// 不满足时返回冲突或清单错误；失败整体回滚（不取号、不写审计）。
func (r *Repository) AutoConfirmHistoricalImportItem(ctx context.Context, tenantID, batchID, itemID string, roles []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与人工确认同一把锁：批次行 FOR UPDATE 保证自动确认与 AdvanceImportConfirmation
		// 串行，不会出现「系统自动批了但人工命令还在追加」的双写。
		var row importBatchRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenantID, batchID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// 已认领的批次在自动确认时消失，只可能是围栏被并发的批次操作改写。
				return application.ErrImportConflict
			}
			return err
		}
		b, err := decodeImportBatch(row)
		if err != nil {
			return err
		}
		index := -1
		for i := range b.Items {
			if b.Items[i].ID == itemID {
				index = i
				break
			}
		}
		if index < 0 {
			return application.ErrImportConflict
		}
		item := &b.Items[index]
		// 幂等：已确认（重跑、或人工会签先到并已完成）直接成功，不重复取号/写审计。
		if item.Confirmed {
			return nil
		}
		// 只有 ARCHIVE_ONLY / HISTORICAL_CLOSED 属于自动审批范围：CONTINUE_LIVE 的
		// 责任激活必须经人工确认链路，NORMAL 根本不走历史确认。
		if item.Item.Mode != contract.ImportArchiveOnly && item.Item.Mode != contract.ImportHistoricalClosed {
			return application.ErrImportUnsupported
		}
		if contract.ValidateHistoryEvidence(item.Item) != nil || item.ContractID == "" {
			return contract.ErrImportManifest
		}
		if b.Confirmation == nil {
			// 合成一份已获批的确认台账：目标投递器（ClaimImportTarget）与前端都按
			// confirmation.state=APPROVED 判断基线已冻结，因此这里必须落成同一形状，
			// 摘要按人工提交时的 {Revision,Items,Sources} 结构计算，保证可核对。
			digest, digestErr := contract.ImportDigest(struct {
				Revision uint64
				Items    []contract.ImportItemState
				Sources  []contract.ImportSource
			}{b.Revision, b.Items, b.Sources})
			if digestErr != nil {
				return digestErr
			}
			now := time.Now().UTC()
			b.Confirmation = &contract.ImportConfirmation{
				WorkflowID:    "contract-import-auto-" + b.ID,
				Revision:      b.Revision,
				Digest:        digest,
				State:         "APPROVED",
				Roles:         roles,
				ApprovedRoles: append([]string(nil), roles...),
				Assignees:     map[string][]string{},
				Commands:      []contract.ImportConfirmationCommand{},
				SubmittedAt:   now,
				ExpiresAt:     now.Add(24 * time.Hour),
			}
		} else if b.Confirmation.State != "APPROVED" {
			// 人工确认已受理但未完成（PENDING/STARTING/REJECTED/...）：自动审批不得
			// 劫持既有会签流程，按冲突交给调度器逐项阻塞并等人工收敛。
			return application.ErrImportConflict
		}
		locked, err := lockImportContractRows(tx, tenantID, []string{item.ContractID})
		if err != nil {
			return err
		}
		if err = confirmHistoricalItem(tx, &b, item, locked); err != nil {
			return err
		}
		// 自动审批是真正的业务写入，必须与合同确认同事务留痕：只记录可核对的标识
		// 与声明状态，不写清单正文、客户名或任何原件内容。
		payload, err := json.Marshal(map[string]any{
			"batch_id":          b.ID,
			"item_id":           item.ID,
			"historical_status": item.Item.HistoryEvidence.HistoricalStatus,
			"source_revision":   item.SourceRevision,
		})
		if err != nil {
			return err
		}
		if err = insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
			ActorID:      "SYSTEM",
			Action:       "CONTRACT_MANAGEMENT:IMPORT_AUTO_APPROVED",
			ResourceType: "contract",
			ResourceID:   item.ContractID,
			Payload:      payload,
		}); err != nil {
			return err
		}
		// 不主动改批次状态：全 COMPLETE 后的收敛（TARGETS_PENDING/CONFIRMATION_PENDING/
		// COMPLETED）由 DispatchImportOnce 统一判定，避免这里与调度器各写一份状态机。
		return saveImportBatch(tx, &b)
	})
}
