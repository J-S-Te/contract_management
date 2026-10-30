package mysql

import (
	"context"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"slices"
	"time"
)

// AdvanceImportConfirmation 应用持久审批命令并原子确认历史合同和逐目标基线。
// 参数：ctx为活动上下文，in为Temporal冻结身份。
// 返回值：当前确认状态或数据库及不可重试基线冲突。
func (r *Repository) AdvanceImportConfirmation(ctx context.Context, in workflows.ImportConfirmationInput) (state string, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", in.TenantID, in.BatchID).Take(&row).Error; e != nil {
			return e
		}
		b, e := decodeImportBatch(row)
		if e != nil {
			return e
		}
		cf := b.Confirmation
		if cf == nil || cf.Digest != in.Digest || cf.Revision != in.Revision || b.Revision != in.Revision {
			return contract.ErrImportManifest
		}
		state = cf.State
		if slices.Contains([]string{"APPROVED", "REJECTED", "WITHDRAWN", "EXPIRED"}, state) {
			return nil
		}
		if !cf.ExpiresAt.After(time.Now().UTC()) {
			cf.State = "EXPIRED"
			if b.State != "COMPLETED" {
				b.State = "PAUSED"
				b.Epoch++
				b.LeaseUntil = time.Time{}
			}
			state = cf.State
			return saveImportBatch(tx, &b)
		}
		for _, cmd := range cf.Commands {
			if cmd.Action == "WITHDRAW" && cmd.ActorID == b.ActorID {
				cf.State = "WITHDRAWN"
				b.State = "PAUSED"
				b.Epoch++
				b.LeaseUntil = time.Time{}
				state = cf.State
				return saveImportBatch(tx, &b)
			}
		}
		for _, item := range b.Items {
			if item.State != "COMPLETE" {
				return nil
			}
		}
		previousState, previousApproved := cf.State, len(cf.ApprovedRoles)
		cf.State = "PENDING"
		for _, cmd := range cf.Commands {
			eligible := false
			for _, role := range cmd.Roles {
				if slices.Contains(cf.Assignees[role], cmd.ActorID) {
					eligible = true
					if cmd.Action == "APPROVE" && !slices.Contains(cf.ApprovedRoles, role) {
						cf.ApprovedRoles = append(cf.ApprovedRoles, role)
					}
				}
			}
			if eligible && cmd.Action == "REJECT" {
				cf.State = "REJECTED"
				break
			}
		}
		if cf.State == "PENDING" && len(cf.Roles) > 0 && len(cf.ApprovedRoles) == len(cf.Roles) {
			for i := range b.Items {
				if e = confirmHistoricalItem(tx, &b, &b.Items[i]); e != nil {
					return e
				}
			}
			cf.State = "APPROVED"
			b.State = "TARGETS_PENDING"
		}
		state = cf.State
		if previousState == cf.State && previousApproved == len(cf.ApprovedRoles) {
			return nil
		}
		return saveImportBatch(tx, &b)
	})
	return
}

// confirmHistoricalItem 仅经独立历史确认写入声明事实及模式终态，不触发普通v1出站。
// 参数：tx为批次事务，b及item为审批冻结基线。
// 返回值：校验、合同并发或审计失败；失败整体回滚。
func confirmHistoricalItem(tx *gorm.DB, b *contract.ImportBatch, item *contract.ImportItemState) error {
	if item.Confirmed {
		return nil
	}
	if item.SourceRevision == 0 || contract.ValidateHistoryEvidence(item.Item) != nil {
		return contract.ErrImportManifest
	}
	var c contractRecord
	if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Omit("rendered_document", "content", "template_values_json").Where("tenant_id=? AND id=?", b.TenantID, item.ContractID).Take(&c).Error; e != nil {
		return e
	}
	if c.Version != 1 || c.OriginKind != "IMPORTED" || c.OriginImportItemID == nil || *c.OriginImportItemID != item.ID || c.SourceFileStatus != "READY" {
		return contract.ErrImportManifest
	}
	now := time.Now().UTC()
	evidence := item.Item.HistoryEvidence
	updates := map[string]any{"status": evidence.HistoricalStatus,
		"version":                2,
		"import_source_revision": item.SourceRevision,
		"import_confirmed_at":    now,
		"updated_at":             now,
		"updated_by":             b.ActorID}
	// 系统号独立分配，原件和纸面编号保持原字节。
	updates["contract_number"] = formatContractNumber(c.ContractNumberFormat, c.ID, now)
	if evidence.DeclaredSignedDate != "" {
		updates["declared_signed_date"] = evidence.DeclaredSignedDate
	}
	if e := tx.Model(&contractRecord{}).Where("tenant_id=? AND id=? AND version=1", b.TenantID, c.ID).Updates(updates).Error; e != nil {
		return e
	}
	item.Confirmed = true
	item.Targets = []contract.ImportTarget{}
	for _, target := range []string{"PROJECT", "FINANCE", "REPORT"} {
		state := "WAITING_RELEASE"
		if (target == "PROJECT" && item.Item.Policy.ProjectMode == "NONE") || (target == "FINANCE" && item.Item.Policy.FinanceMode != "FULL_REPLAY") {
			state = "NOT_APPLICABLE"
		}
		// 当前接收方要求活动责任；结清模式责任必须INACTIVE，不能伪造。
		if target == "FINANCE" && item.Item.Mode == contract.ImportHistoricalClosed && item.Item.Policy.FinanceMode == "FULL_REPLAY" {
			state = "BLOCKED"
		}
		code := ""
		if target == "FINANCE" && item.Item.Policy.FinanceMode == "EVIDENCE_ONLY" {
			code = "IMPORT_FINANCIAL_LEDGER_NOT_REBUILT"
		}
		if state == "BLOCKED" {
			code = "IMPORT_INACTIVE_RESPONSIBILITY_UNSUPPORTED"
		}
		item.Targets = append(item.Targets, contract.ImportTarget{Target: target, State: state, Version: 1, ReleaseVersion: 1, ErrorCode: code})
	}
	payload, _ := contract.ImportDigest(item.Item)
	return insertAuditOutbox(tx, b.TenantID, newID(), AuditOutboxEntry{ActorID: b.ActorID, Action: "CONTRACT_MANAGEMENT:IMPORT_CONFIRMED", ResourceType: "contract", ResourceID: c.ID, Payload: []byte(`{"manifest_digest":"` + payload + `"}`)})
}

// ListImportBatchesPage 有界读取创建人及受派人的候选批次。
// 参数：ctx、tenant限定租户，actor为受信用户，all为预览全范围，limit/offset为分页。
// 返回值：候选批次或存储错误，应用层继续当前权限复核。
func (r *Repository) ListImportBatchesPage(ctx context.Context, tenant, actor string, all bool, limit, offset int) ([]contract.ImportBatch, error) {
	var rows []importBatchRecord
	q := r.db.WithContext(ctx).Where("tenant_id=?", tenant)
	if !all {
		q = q.Where("actor_id=? OR JSON_SEARCH(JSON_EXTRACT(payload,'$.confirmation.assignees'),'one',?) IS NOT NULL", actor, actor)
	}
	if e := q.Order("updated_at DESC,id DESC").Limit(limit).Offset(offset).Find(&rows).Error; e != nil {
		return nil, e
	}
	result := []contract.ImportBatch{}
	for _, row := range rows {
		b, e := decodeImportBatch(row)
		if e != nil {
			return nil, e
		}
		result = append(result, b)
	}
	return result, nil
}

// ListStartingImportConfirmations 恢复持久化已受理但启动响应未知的确认。
// 参数：ctx为上下文。
// 返回值：最多十份待启动台账或读取错误。
func (r *Repository) ListStartingImportConfirmations(ctx context.Context) ([]contract.ImportBatch, error) {
	var rows []importBatchRecord
	e := r.db.WithContext(ctx).Where("JSON_UNQUOTE(JSON_EXTRACT(payload,'$.confirmation.state'))='STARTING'").Order("updated_at").Limit(10).Find(&rows).Error
	result := []contract.ImportBatch{}
	for _, row := range rows {
		b, err := decodeImportBatch(row)
		if err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, e
}
