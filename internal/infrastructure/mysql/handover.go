package mysql

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 交接机器协议的持久化实现：冻结记录、权威责任清单、事务内负责人变更与幂等回执。
// 关键不变量：回执存在 ⇒ 责任变更已在该事务内生效（同事务写入）。

type handoverFreezeRecord struct {
	ID         string     `gorm:"column:id;primaryKey"`
	TenantID   string     `gorm:"column:tenant_id"`
	BatchID    string     `gorm:"column:batch_id"`
	RequestID  string     `gorm:"column:request_id"`
	UserID     string     `gorm:"column:user_id"`
	Status     string     `gorm:"column:status"`
	FrozenAt   time.Time  `gorm:"column:frozen_at"`
	ReleasedAt *time.Time `gorm:"column:released_at"`
	Outcome    *string    `gorm:"column:outcome"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
}

func (handoverFreezeRecord) TableName() string { return "con_handover_freeze" }

type handoverReceiptRecord struct {
	ID             string    `gorm:"column:id;primaryKey"`
	TenantID       string    `gorm:"column:tenant_id"`
	BatchID        string    `gorm:"column:batch_id"`
	ItemID         string    `gorm:"column:item_id"`
	ResourceType   string    `gorm:"column:resource_type"`
	ResourceID     string    `gorm:"column:resource_id"`
	FromUserID     string    `gorm:"column:from_user_id"`
	ToUserID       string    `gorm:"column:to_user_id"`
	IdempotencyKey string    `gorm:"column:idempotency_key"`
	Status         string    `gorm:"column:status"`
	AppliedVersion uint64    `gorm:"column:applied_version"`
	ReasonCode     *string   `gorm:"column:reason_code"`
	EvidenceRef    *string   `gorm:"column:evidence_ref"`
	ReceivedAt     time.Time `gorm:"column:received_at"`
	CreatedAt      time.Time `gorm:"column:created_at"`
}

func (handoverReceiptRecord) TableName() string { return "con_handover_receipt" }

type handoverContractRecord struct {
	ID          string    `gorm:"column:id;primaryKey"`
	TenantID    string    `gorm:"column:tenant_id"`
	Title       string    `gorm:"column:title"`
	OwnerUserID string    `gorm:"column:owner_user_id"`
	Status      string    `gorm:"column:status"`
	Version     uint64    `gorm:"column:version"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (handoverContractRecord) TableName() string { return "con_contract" }

// 审批待办行：running 审批中仍处于 active 的任务。
type handoverApprovalTaskRow struct {
	ApprovalID string     `gorm:"column:approval_id"`
	NodeID     string     `gorm:"column:node_id"`
	NodeName   string     `gorm:"column:node_name"`
	StartedAt  *time.Time `gorm:"column:started_at"`
}

// 未结束合同：归档、已终止与已完成之外的状态都仍需交接当前负责人。
var handoverClosedContractStatuses = []string{"archived", "terminated", "completed"}

var _ application.HandoverRepository = (*Repository)(nil)

// CreateFreeze 幂等创建冻结：同一 (租户, 批次, 用户) 只保留一条。
func (r *Repository) CreateFreeze(ctx context.Context, freeze application.HandoverFreeze) (application.HandoverFreeze, bool, error) {
	record := handoverFreezeRecord{
		ID: freeze.ID, TenantID: freeze.TenantID, BatchID: freeze.BatchID, RequestID: freeze.RequestID,
		UserID: freeze.UserID, Status: freeze.Status, FrozenAt: freeze.FrozenAt.UTC(),
		CreatedAt: freeze.FrozenAt.UTC(), UpdatedAt: freeze.FrozenAt.UTC(),
	}
	if err := r.db.WithContext(ctx).Create(&record).Error; err != nil {
		if !isHandoverDuplicate(err) {
			return application.HandoverFreeze{}, false, err
		}
		var existing handoverFreezeRecord
		if err := r.db.WithContext(ctx).Where("tenant_id = ? AND batch_id = ? AND user_id = ?", freeze.TenantID, freeze.BatchID, freeze.UserID).Take(&existing).Error; err != nil {
			return application.HandoverFreeze{}, false, err
		}
		return handoverFreezeFromRecord(existing), false, nil
	}
	return handoverFreezeFromRecord(record), true, nil
}

// ReleaseFreeze 解除冻结并记录结局；重复解除保持幂等。
func (r *Repository) ReleaseFreeze(ctx context.Context, tenantID, batchID, outcome string, now time.Time) error {
	update := r.db.WithContext(ctx).Model(&handoverFreezeRecord{}).
		Where("tenant_id = ? AND batch_id = ? AND status = ?", tenantID, batchID, application.HandoverFreezeFrozen).
		Updates(map[string]any{"status": application.HandoverFreezeReleased, "released_at": now.UTC(), "outcome": outcome, "updated_at": now.UTC()})
	if update.Error != nil {
		return update.Error
	}
	if update.RowsAffected == 0 {
		var count int64
		if err := r.db.WithContext(ctx).Model(&handoverFreezeRecord{}).Where("tenant_id = ? AND batch_id = ?", tenantID, batchID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return application.ErrHandoverNotFound
		}
	}
	return nil
}

// ActiveFreeze 判断用户当前是否处于交接冻结（写路径据此拒绝新责任分配）。
func (r *Repository) ActiveFreeze(ctx context.Context, tenantID, userID string) (application.HandoverFreeze, bool, error) {
	var record handoverFreezeRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, application.HandoverFreezeFrozen).
		Order("frozen_at DESC").Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.HandoverFreeze{}, false, nil
	}
	if err != nil {
		return application.HandoverFreeze{}, false, err
	}
	return handoverFreezeFromRecord(record), true, nil
}

// ListOwnedResponsibilities 返回该用户作为负责人的未结束合同（按 id 升序分页）。
func (r *Repository) ListOwnedResponsibilities(ctx context.Context, tenantID, userID, cursor string, limit int) ([]application.HandoverResponsibility, error) {
	if limit <= 0 {
		limit = 100
	}
	// 权威责任 = 未结束合同负责人 + 未完成审批节点当前处理人；两类都必须纳入交接。
	contracts, err := r.listContractResponsibilities(ctx, tenantID, userID, cursor, limit)
	if err != nil {
		return nil, err
	}
	approvals, err := r.listApprovalResponsibilities(ctx, tenantID, userID, cursor, limit)
	if err != nil {
		return nil, err
	}
	items := append(contracts, approvals...)
	sort.Slice(items, func(i, j int) bool { return items[i].ResourceID < items[j].ResourceID })
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// listContractResponsibilities 返回该用户负责的未结束合同。
func (r *Repository) listContractResponsibilities(ctx context.Context, tenantID, userID, cursor string, limit int) ([]application.HandoverResponsibility, error) {
	var records []handoverContractRecord
	query := r.db.WithContext(ctx).Model(&handoverContractRecord{}).
		Where("tenant_id = ? AND owner_user_id = ? AND status NOT IN ?", tenantID, userID, handoverClosedContractStatuses)
	if strings.TrimSpace(cursor) != "" {
		query = query.Where("id > ?", strings.TrimSpace(cursor))
	}
	if err := query.Order("id ASC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	items := make([]application.HandoverResponsibility, 0, len(records))
	for _, record := range records {
		items = append(items, application.HandoverResponsibility{
			ResourceType: "contract", ResourceID: record.ID, ResourceName: record.Title,
			OwnerUserID: record.OwnerUserID, Category: application.HandoverCategoryContractOwner,
			Version: record.Version, UpdatedAt: record.UpdatedAt,
		})
	}
	return items, nil
}

// TransferOwnership 在本地事务内变更负责人、递增版本并写入幂等回执。
func (r *Repository) TransferOwnership(ctx context.Context, in application.HandoverTransferInput, now time.Time) (application.HandoverReceipt, error) {
	var receipt application.HandoverReceipt
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing handoverReceiptRecord
		err := tx.Where("tenant_id = ? AND idempotency_key = ?", in.TenantID, in.IdempotencyKey).Take(&existing).Error
		if err == nil {
			receipt = handoverReceiptFromRecord(existing)
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var contractRow handoverContractRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", in.TenantID, in.ResourceID).Take(&contractRow).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return application.ErrHandoverNotFound
			}
			return err
		}
		// 版本或当前负责人不符：写入 REJECTED 回执让平台看到确定性结论，而不是猜测。
		if contractRow.Version != in.ExpectedVersion || contractRow.OwnerUserID != in.FromUserID {
			rejected := handoverReceiptRecord{
				ID: ulid.Make().String(), TenantID: in.TenantID, BatchID: in.BatchID, ItemID: in.ItemID,
				ResourceType: in.ResourceType, ResourceID: in.ResourceID, FromUserID: in.FromUserID, ToUserID: in.ToUserID,
				IdempotencyKey: in.IdempotencyKey, Status: application.HandoverReceiptRejected,
				ReasonCode: optionalString("CONTRACT_VERSION_CONFLICT"), ReceivedAt: now.UTC(), CreatedAt: now.UTC(),
			}
			if err := tx.Create(&rejected).Error; err != nil {
				return err
			}
			receipt = handoverReceiptFromRecord(rejected)
			return nil
		}

		updates := map[string]any{
			// SELF 数据范围读取 owner_identity_id；用户 ID 与身份 ID 在本系统
			// 来自同一平台主体，必须同步迁移，否则承接人看不到合同、前任仍可见。
			"owner_user_id": in.ToUserID, "owner_identity_id": in.ToUserID,
			"version":    gorm.Expr("version + 1"),
			"updated_at": now.UTC(), "updated_by": in.ToUserID,
		}
		// 权威目录未接线时保留旧展示名，绝不用空值抹掉；接线后必须同步新姓名。
		if strings.TrimSpace(in.ToDisplayName) != "" {
			updates["owner_display_name"] = in.ToDisplayName
		}
		update := tx.Model(&handoverContractRecord{}).
			Where("tenant_id = ? AND id = ? AND version = ? AND owner_user_id = ?", in.TenantID, in.ResourceID, in.ExpectedVersion, in.FromUserID).
			Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return application.ErrHandoverConflict
		}

		applied := handoverReceiptRecord{
			ID: ulid.Make().String(), TenantID: in.TenantID, BatchID: in.BatchID, ItemID: in.ItemID,
			ResourceType: in.ResourceType, ResourceID: in.ResourceID, FromUserID: in.FromUserID, ToUserID: in.ToUserID,
			IdempotencyKey: in.IdempotencyKey, Status: application.HandoverReceiptApplied,
			AppliedVersion: in.ExpectedVersion + 1, EvidenceRef: optionalString("contract:" + in.ResourceID),
			ReceivedAt: now.UTC(), CreatedAt: now.UTC(),
		}
		if err := tx.Create(&applied).Error; err != nil {
			return err
		}
		receipt = handoverReceiptFromRecord(applied)
		return nil
	})
	if err != nil {
		return application.HandoverReceipt{}, err
	}
	return receipt, nil
}

// listApprovalResponsibilities 查询该用户当前处理中的审批节点：
// 审批实例必须处于 running、任务处于 active，且租户来自平台令牌绑定。
func (r *Repository) listApprovalResponsibilities(ctx context.Context, tenantID, userID, cursor string, limit int) ([]application.HandoverResponsibility, error) {
	query := r.db.WithContext(ctx).Table("con_approval_task AS task").
		Select("task.approval_id, task.node_id, task.node_name, task.started_at").
		Joins("JOIN con_approval_instance AS approval ON approval.id = task.approval_id AND approval.tenant_id = ?", tenantID).
		Where("task.assignee_user_id = ? AND task.status = ? AND approval.status = ?", userID, "active", "running")
	if strings.TrimSpace(cursor) != "" {
		query = query.Where("CONCAT(task.approval_id, ':', task.node_id) > ?", strings.TrimSpace(cursor))
	}
	var rows []handoverApprovalTaskRow
	if err := query.Order("task.approval_id ASC, task.node_id ASC").Limit(limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]application.HandoverResponsibility, 0, len(rows))
	for _, row := range rows {
		updated := time.Time{}
		if row.StartedAt != nil {
			updated = *row.StartedAt
		}
		items = append(items, application.HandoverResponsibility{
			ResourceType: application.HandoverResourceApprovalTask,
			ResourceID:   row.ApprovalID + ":" + row.NodeID,
			ResourceName: row.NodeName, OwnerUserID: userID,
			Category: application.HandoverCategoryApprovalAssignee, UpdatedAt: updated,
		})
	}
	return items, nil
}

// SaveReceipt 持久化已经生效的审批工作流转派回执；同幂等键重复写入保持幂等。
func (r *Repository) SaveReceipt(ctx context.Context, receipt application.HandoverReceipt) error {
	record := handoverReceiptRecord{
		ID: ulid.Make().String(), TenantID: receipt.TenantID, BatchID: receipt.BatchID, ItemID: receipt.ItemID,
		ResourceType: receipt.ResourceType, ResourceID: receipt.ResourceID,
		FromUserID: receipt.FromUserID, ToUserID: receipt.ToUserID,
		IdempotencyKey: receipt.IdempotencyKey, Status: receipt.Status,
		AppliedVersion: receipt.AppliedVersion, ReasonCode: optionalString(receipt.ReasonCode),
		EvidenceRef: optionalString(receipt.EvidenceRef), ReceivedAt: receipt.ReceivedAt.UTC(), CreatedAt: time.Now().UTC(),
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "tenant_id"}, {Name: "idempotency_key"}},
		DoNothing: true,
	}).Create(&record).Error
}

// FindReceipt 按幂等键查询既有回执。
func (r *Repository) FindReceipt(ctx context.Context, tenantID, idempotencyKey string) (application.HandoverReceipt, bool, error) {
	var record handoverReceiptRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND idempotency_key = ?", tenantID, idempotencyKey).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.HandoverReceipt{}, false, nil
	}
	if err != nil {
		return application.HandoverReceipt{}, false, err
	}
	return handoverReceiptFromRecord(record), true, nil
}

func handoverFreezeFromRecord(record handoverFreezeRecord) application.HandoverFreeze {
	outcome := ""
	if record.Outcome != nil {
		outcome = *record.Outcome
	}
	return application.HandoverFreeze{
		ID: record.ID, TenantID: record.TenantID, BatchID: record.BatchID, RequestID: record.RequestID,
		UserID: record.UserID, Status: record.Status, FrozenAt: record.FrozenAt, ReleasedAt: record.ReleasedAt, Outcome: outcome,
	}
}

func handoverReceiptFromRecord(record handoverReceiptRecord) application.HandoverReceipt {
	reason, evidence := "", ""
	if record.ReasonCode != nil {
		reason = *record.ReasonCode
	}
	if record.EvidenceRef != nil {
		evidence = *record.EvidenceRef
	}
	return application.HandoverReceipt{
		TenantID: record.TenantID, BatchID: record.BatchID, ItemID: record.ItemID,
		ResourceType: record.ResourceType, ResourceID: record.ResourceID,
		FromUserID: record.FromUserID, ToUserID: record.ToUserID,
		IdempotencyKey: record.IdempotencyKey, Status: record.Status,
		AppliedVersion: record.AppliedVersion, ReasonCode: reason, EvidenceRef: evidence, ReceivedAt: record.ReceivedAt,
	}
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(value)
	return &trimmed
}

// isHandoverDuplicate 识别唯一键冲突；合同仓历史上按驱动文本判断，这里保持一致并兼容 GORM 翻译。
func isHandoverDuplicate(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate")
}
