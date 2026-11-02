package mysql

import (
	"context"
	"fmt"
	"github.com/j-s-te/contract-management/internal/apperrors"
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
			// 两阶段确认的第一步：先按 id 升序一次性锁住本批全部目标合同行。
			// 必须先锁全部合同行，再进入逐条取号（详见 lockImportContractRows 注释）。
			targets := make([]string, 0, len(b.Items))
			for i := range b.Items {
				if !b.Items[i].Confirmed && b.Items[i].ContractID != "" {
					targets = append(targets, b.Items[i].ContractID)
				}
			}
			locked, e := lockImportContractRows(tx, b.TenantID, targets)
			if e != nil {
				return e
			}
			// 第二步：逐条确认；此时合同行锁全部在手，循环内只会再申请流水行锁。
			for i := range b.Items {
				if e = confirmHistoricalItem(tx, &b, &b.Items[i], locked); e != nil {
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

// lockImportContractRows 按主键升序一次性锁住本批待确认的全部合同行，并返回已锁定的合同 ID 集合。
//
// 为什么必须先锁全部合同行再取号：
//
// 取号 allocateContractNumberInTx 会在本事务内对 con_contract_number_sequence 的
// (seq_year, rule_key) 行加 X 锁，并持有到事务结束。普通审批路径的加锁顺序是
// 「合同行 → 流水行」（repository.go CompleteApproval：先 FOR UPDATE 锁合同行，再取号）。
// 如果批次事务逐条执行「第 1 条合同行 → 流水行 → 第 2 条合同行 → ……」，
// 它对第 2..N 条合同行的加锁就排在流水行之后，与审批事务的「合同行 → 流水行」
// 恰好相反：审批持有后段合同行等流水行，批次持有流水行等后段合同行，形成环，
// InnoDB 以 1213 死锁回滚其中一方。
//
// 一次性按 id 升序锁住全部目标合同行后，批次事务的全局加锁顺序固定为
// 「全部合同行（id 升序）→ 流水行」，与审批同向；任何事务都不可能在持有流水行锁时
// 反向等待本批合同行锁，因此环路不可能闭合。禁止改回循环内逐条加锁。
//
// 锁获取顺序说明：id 是 con_contract 的主键，SELECT ... WHERE id IN (...) ORDER BY id
// 走主键范围扫描，加锁顺序即索引升序；同一批合同行对所有事务都是同一个顺序。
func lockImportContractRows(tx *gorm.DB, tenantID string, ids []string) (map[string]bool, error) {
	locked := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return locked, nil
	}
	var found []string
	if err := tx.Model(&contractRecord{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id=? AND id IN ?", tenantID, ids).Order("id ASC").Pluck("id", &found).Error; err != nil {
		return nil, err
	}
	for _, id := range found {
		locked[id] = true
	}
	return locked, nil
}

// confirmHistoricalItem 仅经独立历史确认写入声明事实及模式终态，不触发普通v1出站。
// 参数：tx为批次事务，b及item为审批冻结基线，locked为批次事务前置锁定的合同 ID 集合。
// 返回值：校验、合同并发或审计失败；失败整体回滚。
func confirmHistoricalItem(tx *gorm.DB, b *contract.ImportBatch, item *contract.ImportItemState, locked map[string]bool) error {
	if item.Confirmed {
		return nil
	}
	if item.SourceRevision == 0 || contract.ValidateHistoryEvidence(item.Item) != nil {
		return contract.ErrImportManifest
	}
	// 合同行必须已由批次事务的前置阶段独占锁定。这里刻意不再用 FOR UPDATE 补锁：
	// 一旦补锁就可能在持有流水行锁之后再申请合同行锁，与审批路径反向成环。
	// 前置锁持有到事务结束，因此本事务内的一致读只能读到本事务独占的那一行版本，
	// 版本与来源硬校验的语义不变。未覆盖即说明批次基线与合同集合不一致，按基线冲突终止。
	if !locked[item.ContractID] {
		return contract.ErrImportManifest
	}
	var c contractRecord
	if e := tx.Omit("rendered_document", "content", "template_values_json").Where("tenant_id=? AND id=?", b.TenantID, item.ContractID).Take(&c).Error; e != nil {
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
	// 系统号独立分配，原件和纸面编号保持原字节；采用系统级规则，规则缺失时回退旧格式。
	number, err := allocateContractNumberInTx(tx, b.TenantID, c.ID, c.ContractNumberFormat, now)
	if err != nil {
		return err
	}
	updates["contract_number"] = number
	if evidence.DeclaredSignedDate != "" {
		updates["declared_signed_date"] = evidence.DeclaredSignedDate
	}
	updated := tx.Model(&contractRecord{}).Where("tenant_id=? AND id=? AND version=1", b.TenantID, c.ID).Updates(updates)
	if e := updated.Error; e != nil {
		// 与审批路径同一判定：命中 uk_con_contract_number(tenant_id, contract_number)
		// 说明该租户已存在同号合同，重试永远不会成功，必须归为确定性编号冲突业务错误，
		// 否则导入确认活动会拿着可重试错误无限重试、批次永远收不了口。
		if isContractNumberDuplicate(e) {
			return fmt.Errorf("%w: %s 已被本租户的另一份合同占用，请执行 cmd/contract-number-audit 核对全系统编号，受控更正后再重试历史导入确认",
				contract.ErrNumberConflict, number)
		}
		return e
	}
	// 与审批路径对齐：编号已由登记表占号并可能已改写正文，更新必须恰好命中一行。
	// 命中 0 行说明合同行在事务内被改动（版本/归属不再匹配），必须回滚以免登记表占号而合同无号。
	if updated.RowsAffected != 1 {
		return fmt.Errorf("%w: 历史合同 %s 的确认更新未命中唯一一行（影响 %d 行），已回滚本次编号分配",
			apperrors.ErrStateConflict, c.ID, updated.RowsAffected)
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
