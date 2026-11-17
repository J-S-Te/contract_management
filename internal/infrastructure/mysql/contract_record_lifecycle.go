package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const recordSummaryColumns = "id,tenant_id,contract_number,title,contract_type,customer_name," +
	"owner_display_name,owner_identity_id,owner_org_id,project_id,amount_minor,currency," +
	"status,version,deleted_at,deleted_by,delete_reason,updated_at"

// GetContractHistoryMetadata 用途：为不可变审批历史保留当前归属授权，禁止读取墓碑正文。
// 参数：ctx 为请求，filter 为当前读取范围，id 为审批所属合同。
// 返回值：有限合同元数据；范围外和不存在都返回资源不存在。
func (r *Repository) GetContractHistoryMetadata(ctx context.Context, filter contract.ScopeFilter, id string) (contract.Contract, error) {
	var row contractRecord
	err := applyContractScope(r.db.WithContext(ctx).Unscoped().Model(&contractRecord{}), filter).
		Select(recordSummaryColumns).Where("id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.Contract{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.Contract{}, err
	}
	return contractFromRecord(row), nil
}

type recordEvidence struct {
	InFlight, Approved, Project, Finance int64
	ImportSettled                        bool
}

// contractRecordIsDeleted 用途：在批次到合同的既有锁序内核对确定墓碑终态。
// 参数：tx 为持有批次锁的事务，tenant/id 为任务原始源身份。
// 返回值：真实删除标记或缺失、存储错误；绝不重建合同身份。
func contractRecordIsDeleted(tx *gorm.DB, tenant, id string) (bool, error) {
	var row contractRecord
	err := tx.Unscoped().Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "deleted_at").Where("tenant_id=? AND id=?", tenant, id).Take(&row).Error
	return row.DeletedAt.Valid, err
}

// managedContractQuery 用途：读取和管理范围取交集，显式允许查询墓碑。
// 参数：tx 为当前连接，read/manage 为可信范围，id 为唯一合同身份。
// 返回值：包含租户与双重行级授权的查询，不暴露范围外记录是否存在。
func managedContractQuery(tx *gorm.DB, read, manage contract.ScopeFilter, id string) *gorm.DB {
	return applyContractScope(applyContractScope(tx.Unscoped().Model(&contractRecord{}), read), manage).
		Where("id = ?", id)
}

// recordSummary 用途：生成不含正文或原件的管理摘要。
// 参数：row 为已完成范围过滤的合同主行。
// 返回值：包含记录状态和删除元数据的安全摘要。
func recordSummary(row contractRecord) contract.RecordSummary {
	result := contract.RecordSummary{ID: row.ID, Number: valueOrEmpty(row.ContractNumber),
		Title: row.Title, Type: row.ContractType, CustomerName: valueOrEmpty(row.CustomerName),
		OwnerDisplayName: row.OwnerDisplayName, AmountMinor: row.AmountMinor, Currency: row.Currency,
		Status: contract.Status(row.Status), Version: row.Version, RecordState: contract.RecordActive,
		UpdatedAt: row.UpdatedAt}
	if row.DeletedAt.Valid {
		at := row.DeletedAt.Time
		result.RecordState, result.DeletedAt = contract.RecordDeleted, &at
		result.DeletedBy, result.DeleteReason = valueOrEmpty(row.DeletedBy), valueOrEmpty(row.DeleteReason)
	}
	return result
}

// recordSafetyBlockers 用途：根据本地真实证据限制删除和恢复为不会派生业务的候选。
// 参数：row 为权威合同，facts 为同一事务读取的审批、投递和来源收口证据。
// 返回值：明确阻断项；统计、通知、责任元数据与报表不作为业务关联。
func recordSafetyBlockers(row contractRecord, facts recordEvidence) []contract.RecordBlocker {
	blockers := make([]contract.RecordBlocker, 0)
	if facts.InFlight > 0 {
		blockers = append(blockers, contract.RecordBlocker{Code: "APPROVAL_IN_FLIGHT", Message: "审批仍在执行或完成结果未落账，请先在审批中心收口。"})
	}
	if valueOrEmpty(row.ProjectID) != "" || facts.Project > 0 {
		blockers = append(blockers, contract.RecordBlocker{Code: "PROJECT_ASSOCIATED", Message: "已有项目关联或项目投递记录，请保留合同并按业务流程作废或终止。"})
	}
	if facts.Finance > 0 {
		blockers = append(blockers, contract.RecordBlocker{Code: "FINANCE_ASSOCIATED", Message: "已有财务投递或资金关联，请先按结算业务流程处理，不能删除合同。"})
	}
	if !facts.ImportSettled {
		blockers = append(blockers, contract.RecordBlocker{Code: "IMPORT_NOT_SETTLED", Message: "合同导入来源尚未收口或证据不完整，请先核对原导入批次。"})
	}
	if !recordCannotDerive(row, facts.Approved) {
		blockers = append(blockers, contract.RecordBlocker{Code: "DOWNSTREAM_UNKNOWN", Message: "合同曾具备业务派生资格或来源策略不完整，当前无法可靠排除项目、补充协议或资金关联；请保留记录并核对业务历史。"})
	}
	return blockers
}

// recordCannotDerive 用途：限制恢复升版本后仍不会进入项目或财务补建路径。
// 参数：row 为主行，approved 为历史已批准或已激活的证据数量。
// 返回值：从未批准的普通草稿，或已确认且明确不派生的历史策略返回真。
func recordCannotDerive(row contractRecord, approved int64) bool {
	if ordinaryContractRecord(row) {
		return row.Status == string(contract.StatusDraft) && approved == 0
	}
	if row.OriginKind != "IMPORTED" || row.ImportConfirmedAt == nil {
		return false
	}
	var policy contract.ImportPolicy
	if json.Unmarshal(row.ImportPolicyJSON, &policy) != nil || contract.ValidateImportPolicy(contract.ImportMode(row.ImportMode), &policy) != nil {
		return false
	}
	return policy.ProjectMode == "NONE" && (policy.FinanceMode == "NONE" || policy.FinanceMode == "EVIDENCE_ONLY")
}

// loadRecordEvidence 用途：在本地事务内读取有限业务证据，不等待远端或反向锁上游。
// 参数：tx 为当前事务，row 为已锁定或快照读取的合同。
// 返回值：审批及项目、财务的真实证据；查询失败原样返回，不伪造零关联。
func loadRecordEvidence(tx *gorm.DB, row contractRecord) (recordEvidence, error) {
	hasSourceItem := row.OriginImportItemID != nil && *row.OriginImportItemID != ""
	facts := recordEvidence{ImportSettled: ordinaryContractRecord(row) && !hasSourceItem}
	base := "tenant_id = ? AND contract_id = ?"
	if err := tx.Model(&approvalInstanceRecord{}).Where(base, row.TenantID, row.ID).
		Where("status = 'running' OR completion_applied = false OR active_status_change_key IS NOT NULL").Count(&facts.InFlight).Error; err != nil {
		return facts, err
	}
	if err := tx.Model(&approvalInstanceRecord{}).Where(base, row.TenantID, row.ID).
		Where("status = 'approved'").Count(&facts.Approved).Error; err != nil {
		return facts, err
	}
	var lifecycle int64
	if err := tx.Model(&lifecycleEventRecord{}).Where(base, row.TenantID, row.ID).
		Where("to_status IN ('approved','active','in_progress','pending_pay','completed','terminated','archived')").Count(&lifecycle).Error; err != nil {
		return facts, err
	}
	facts.Approved += lifecycle
	if err := tx.Model(&projectDeliveryOutboxRecord{}).Where(base, row.TenantID, row.ID).Count(&facts.Project).Error; err != nil {
		return facts, err
	}
	if err := tx.Model(&financialEffectiveOutboxRecord{}).Where(base, row.TenantID, row.ID).Count(&facts.Finance).Error; err != nil {
		return facts, err
	}
	if hasSourceItem {
		settled, project, finance, err := recordImportEvidence(tx, row)
		facts.ImportSettled, facts.Project, facts.Finance = settled, facts.Project+project, facts.Finance+finance
		if err != nil {
			return facts, err
		}
	}
	return facts, nil
}

// recordImportEvidence 用途：核对原导入项已完成，且没有项目或财务目标的执行事实。
// 参数：tx 为当前事务，row 提供可信租户、合同与来源项身份。
// 返回值：来源是否收口、项目和财务关联数；缺失或歧义视为未收口。
func recordImportEvidence(tx *gorm.DB, row contractRecord) (bool, int64, int64, error) {
	var batches []importBatchRecord
	err := tx.Where("tenant_id=? AND JSON_CONTAINS(payload, JSON_OBJECT('id', ?), '$.items')", row.TenantID, *row.OriginImportItemID).
		Limit(2).Find(&batches).Error
	if err != nil || len(batches) != 1 {
		return false, 0, 0, err
	}
	batch, err := decodeImportBatch(batches[0])
	if err != nil {
		return false, 0, 0, err
	}
	for _, item := range batch.Items {
		if item.ID != *row.OriginImportItemID {
			continue
		}
		settled := item.ContractID == row.ID && item.State == "COMPLETE"
		if !ordinaryContractRecord(row) {
			settled = settled && recordHistoricalSourceMatches(batch, item, row)
		}
		var project, finance int64
		for _, target := range item.Targets {
			if target.State == "NOT_APPLICABLE" {
				continue
			}
			if target.Target == "PROJECT" {
				project++
			}
			if target.Target == "FINANCE" {
				finance++
			}
		}
		return settled, project, finance, nil
	}
	return false, 0, 0, nil
}

// recordHistoricalSourceMatches 用途：核对实际获准来源，不能只凭主行模式名放行。
// 参数：batch/item 为原冻结来源，row 为待管理的历史合同主行。
// 返回值：已批准确认、状态、来源修订和有效策略完全一致时为真。
func recordHistoricalSourceMatches(batch contract.ImportBatch, item contract.ImportItemState, row contractRecord) bool {
	if batch.Confirmation == nil || batch.Confirmation.State != "APPROVED" || !item.Confirmed ||
		item.SourceRevision != row.ImportSourceRevision || string(item.Item.Mode) != row.ImportMode ||
		contract.ValidateHistoryEvidence(item.Item) != nil || item.Item.HistoryEvidence.HistoricalStatus != row.Status {
		return false
	}
	var policy contract.ImportPolicy
	if json.Unmarshal(row.ImportPolicyJSON, &policy) != nil {
		return false
	}
	expected, expectedErr := contract.ImportDigest(item.Item.Policy)
	actual, actualErr := contract.ImportDigest(&policy)
	return expectedErr == nil && actualErr == nil && expected == actual
}

// PreviewContractRecord 用途：以一致性快照读取当前删除或恢复资格。
// 参数：ctx 控制读取期限，read/manage 为双范围，id 为合同身份。
// 返回值：安全摘要及阻断项；范围外与不存在均返回资源不存在。
func (r *Repository) PreviewContractRecord(ctx context.Context, read, manage contract.ScopeFilter, id string) (result contract.RecordPreview, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row contractRecord
		if e := managedContractQuery(tx, read, manage, id).Omit("rendered_document", "content", "template_values_json").Take(&row).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			return e
		}
		facts, e := loadRecordEvidence(tx, row)
		if e != nil {
			return e
		}
		result.RecordSummary, result.Blockers = recordSummary(row), recordSafetyBlockers(row, facts)
		result.CanDelete = !row.DeletedAt.Valid && len(result.Blockers) == 0
		result.CanRestore = row.DeletedAt.Valid && len(result.Blockers) == 0
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}

// ChangeContractRecord 用途：锁定父合同后复核资格，同事务写状态、生命周期和责任审计。
// 参数：ctx 为请求，read/manage 为双范围，id/version 为所见基线，actor/reason 为审计说明，target 为记录目标状态。
// 返回值：准确的幂等回执或冲突；同目标重放不重复递增版本及写出站事件。
func (r *Repository) ChangeContractRecord(ctx context.Context, read, manage contract.ScopeFilter, id string, version uint64, actor, reason string, target contract.RecordState) (result contract.RecordMutation, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row contractRecord
		if e := managedContractQuery(tx, read, manage, id).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			return e
		}
		state := contract.RecordActive
		if row.DeletedAt.Valid {
			state = contract.RecordDeleted
		}
		result = contract.RecordMutation{ID: row.ID, Version: row.Version, RecordState: state}
		if state == target {
			return nil
		}
		if row.Version != version {
			return apperrors.ErrVersionConflict
		}
		facts, e := loadRecordEvidence(tx, row)
		if e != nil {
			return e
		}
		if blockers := recordSafetyBlockers(row, facts); len(blockers) > 0 {
			return &application.ContractDeleteBlockedError{Blockers: blockers}
		}
		if e = writeRecordChange(tx, row, actor, reason, target); e != nil {
			return e
		}
		result.Version, result.RecordState, result.Changed = row.Version+1, target, true
		return nil
	})
	return result, err
}

// writeRecordChange 用途：保留业务状态与永久身份，仅切换墓碑并记录单次动作。
// 参数：tx 持有父行锁，row 为原记录，actor/reason 为操作依据，target 为记录状态。
// 返回值：任一写入失败导致全事务回滚，不产生审批、项目或财务事件。
func writeRecordChange(tx *gorm.DB, row contractRecord, actor, reason string, target contract.RecordState) error {
	now, eventType := time.Now().UTC(), "RESTORE"
	values := map[string]any{"deleted_at": nil, "deleted_by": nil, "delete_reason": nil,
		"version": row.Version + 1, "updated_at": now, "updated_by": actor}
	if target == contract.RecordDeleted {
		eventType = "DELETE"
		values["deleted_at"], values["deleted_by"], values["delete_reason"] = now, actor, reason
	}
	updated := tx.Unscoped().Model(&contractRecord{}).Where("tenant_id=? AND id=? AND version=?", row.TenantID, row.ID, row.Version).Updates(values)
	if updated.Error != nil {
		return updated.Error
	}
	if updated.RowsAffected != 1 {
		return apperrors.ErrVersionConflict
	}
	if err := tx.Create(&lifecycleEventRecord{ID: newID(), TenantID: row.TenantID, ContractID: row.ID,
		EventType: eventType, FromStatus: row.Status, ToStatus: row.Status, ActorUserID: stringPtr(actor),
		Reason: stringPtr(reason), IdempotencyKey: fmt.Sprintf("%s:record:%d", row.ID, row.Version+1), OccurredAt: now}).Error; err != nil {
		return err
	}
	if err := enqueueContractResponsibility(tx, row.TenantID, row.ID); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"contract_id": row.ID, "record_state": target, "version": row.Version + 1, "reason": reason})
	if err != nil {
		return err
	}
	meta := auditctx.FromContext(tx.Statement.Context)
	return insertAuditOutbox(tx, row.TenantID, newID(), AuditOutboxEntry{ActorID: actor,
		Action: "CONTRACT_MANAGEMENT:" + eventType, ResourceType: "contract", ResourceID: row.ID,
		Payload: payload, LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID})
}

// ListDeletedContractRecords 用途：分页读取墓碑摘要，原件与正文不进入回收站。
// 参数：ctx 为请求，read/manage 为双范围，query 为现有台账筛选。
// 返回值：同一快照和谓词下的总数与摘要页，失败不伪造空集合。
func (r *Repository) ListDeletedContractRecords(ctx context.Context, read, manage contract.ScopeFilter, query contract.ContractListQuery) (page contract.DeletedRecordPage, err error) {
	query = contractLedgerPagingDefaults(query)
	page.Items = []contract.RecordSummary{}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		base := func() *gorm.DB {
			return contractLedgerFilters(applyContractScope(applyContractScope(tx.Unscoped().Model(&contractRecord{}), read), manage), query).Where("deleted_at IS NOT NULL")
		}
		if e := base().Count(&page.Total).Error; e != nil {
			return e
		}
		offset := contract.ClampContractListOffset(query.Offset, page.Total)
		var rows []contractRecord
		if e := base().Select(recordSummaryColumns).Order("deleted_at DESC, id DESC").Offset(offset).Limit(query.Limit).Find(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			page.Items = append(page.Items, recordSummary(row))
		}
		page.Page, page.PageSize = offset/query.Limit+1, query.Limit
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return page, err
}
