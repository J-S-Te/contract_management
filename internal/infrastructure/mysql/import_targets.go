package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// GetImportItemBatch 通过租户及不可变itemID定位完整批次。
// 参数：ctx为上下文，tenant/item限定来源身份。
// 返回值：台账或未找到、数据库错误。
func (r *Repository) GetImportItemBatch(ctx context.Context, tenant, item string) (contract.ImportBatch, error) {
	var row importBatchRecord
	if e := r.db.WithContext(ctx).Where("tenant_id=? AND JSON_SEARCH(payload,'one',?,NULL,'$.items[*].id') IS NOT NULL", tenant, item).Take(&row).Error; e != nil {
		if errors.Is(e, gorm.ErrRecordNotFound) {
			return contract.ImportBatch{}, apperrors.ErrNotFound
		}
		return contract.ImportBatch{}, e
	}
	return decodeImportBatch(row)
}

// QueueImportTarget 持锁更新逐目标任务，REPORT发布列与任务在同事务写入。
// 参数：ctx及tenant/batch/item限定目标，version是目标版本，target为业务目标，change不调用远端。
// 返回值：版本、策略、存储错误。
func (r *Repository) QueueImportTarget(ctx context.Context, tenant, batch, item string, version uint64, target string, change func(*contract.ImportBatch, *contract.ImportItemState, *contract.ImportTarget) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, batch).Take(&row).Error; e != nil {
			return e
		}
		b, e := decodeImportBatch(row)
		if e != nil {
			return e
		}
		for i := range b.Items {
			it := &b.Items[i]
			if it.ID != item {
				continue
			}
			for j := range it.Targets {
				t := &it.Targets[j]
				if t.Target != target {
					continue
				}
				if t.Version != version {
					return application.ErrImportConflict
				}
				oldGeneration := t.CommandGeneration
				if e = change(&b, it, t); e != nil {
					return e
				}
				t.Version++
				if t.CommandGeneration != oldGeneration {
					if t.CommandGeneration != oldGeneration+1 || t.RecoveryRequestedBy == "" || len(t.CommandHistory) == 0 {
						return application.ErrImportConflict
					}
					old := t.CommandHistory[len(t.CommandHistory)-1]
					payload, e := json.Marshal(map[string]any{"item_id": it.ID, "target": target, "old_command_id": old.CommandID, "old_request_digest": old.RequestDigest, "rejected_http_status": old.HTTPStatus, "recipient_code": old.RecipientCode, "command_id": t.CommandID, "command_generation": t.CommandGeneration})
					if e != nil {
						return e
					}
					if e = insertAuditOutbox(tx, tenant, newID(), AuditOutboxEntry{ActorID: t.RecoveryRequestedBy, Action: "CONTRACT_MANAGEMENT:IMPORT_FINANCE_RELEASE_RETRY_AUTHORIZED", ResourceType: "contract", ResourceID: it.ContractID, Payload: payload}); e != nil {
						return e
					}
					b.State = "TARGETS_PENDING"
				}
				if target == "REPORT" && t.State == "QUEUED" {
					t.State = "RELEASE_REQUESTED"
					result := tx.Model(&contractRecord{}).Where("tenant_id=? AND id=? AND origin_import_item_id=? AND import_source_revision=? AND import_confirmed_at IS NOT NULL", tenant, it.ContractID, item, it.SourceRevision).Updates(map[string]any{"import_report_release_version": t.ReleaseVersion, "import_report_required_channels": it.Item.HistoryEvidence.ReportRequiredChannels})
					if result.Error != nil {
						return result.Error
					}
					if result.RowsAffected != 1 {
						return application.ErrImportConflict
					}
				}
				return saveImportBatch(tx, &b)
			}
		}
		return application.ErrImportConflict
	})
}

// ClaimImportTarget 使用批次行锁和目标epoch认领独立持久任务。
// 参数：ctx为上下文，now为租约基准。
// 返回值：冻结批次、itemID、目标、是否认领及存储错误。
func (r *Repository) ClaimImportTarget(ctx context.Context, now time.Time) (b contract.ImportBatch, item, target string, found bool, err error) {
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("JSON_UNQUOTE(JSON_EXTRACT(payload,'$.confirmation.state'))='APPROVED'").Where("JSON_SEARCH(payload,'one','QUEUED',NULL,'$.items[*].targets[*].state') IS NOT NULL OR JSON_SEARCH(payload,'one','WAITING',NULL,'$.items[*].targets[*].state') IS NOT NULL OR JSON_SEARCH(payload,'one','RUNNING',NULL,'$.items[*].targets[*].state') IS NOT NULL OR JSON_SEARCH(payload,'one','RELEASE_REQUESTED',NULL,'$.items[*].targets[*].state') IS NOT NULL").Order("updated_at").Limit(20).Find(&rows).Error; e != nil {
			return e
		}
		for _, row := range rows {
			current, e := decodeImportBatch(row)
			if e != nil {
				return e
			}
			for i := range current.Items {
				for j := range current.Items[i].Targets {
					t := &current.Items[i].Targets[j]
					if (t.State != "QUEUED" && t.State != "WAITING" && t.State != "RUNNING" && t.State != "RELEASE_REQUESTED") || t.LeaseUntil.After(now) || t.NextAttemptAt.After(now) {
						continue
					}
					t.State = "RUNNING"
					t.Epoch++
					t.Attempts++
					t.Version++
					t.LeaseUntil = now.Add(time.Minute)
					item, target, found = current.Items[i].ID, t.Target, true
					if e = saveImportBatch(tx, &current); e != nil {
						return e
					}
					b = current
					return nil
				}
			}
		}
		return nil
	})
	return
}

// SaveImportTarget 仅允许当前有限租约代次保存回执及恢复状态。
// 参数：ctx及tenant/batch/item限定源身份，task携带目标epoch。
// 返回值：过期工作器冲突或持久化错误。
func (r *Repository) SaveImportTarget(ctx context.Context, tenant, batch, item string, task contract.ImportTarget) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, batch).Take(&row).Error; e != nil {
			return e
		}
		b, e := decodeImportBatch(row)
		if e != nil {
			return e
		}
		if e = applyImportTargetResult(tx, &b, item, task); e != nil {
			return e
		}
		return saveImportBatch(tx, &b)
	})
}

// applyImportTargetResult 在有限租约内保存同命令结果并原子记录真实拒绝审计。
// 参数：tx/b为持锁批次，item为源单项，task为当前epoch的实际执行结果。
// 返回值：迟到、异命令或审计失败均拒绝整笔保存。
func applyImportTargetResult(tx *gorm.DB, b *contract.ImportBatch, item string, task contract.ImportTarget) error {
	for i := range b.Items {
		if b.Items[i].ID != item {
			continue
		}
		for j := range b.Items[i].Targets {
			current := &b.Items[i].Targets[j]
			if current.Target != task.Target {
				continue
			}
			if current.Epoch != task.Epoch || current.CommandID != task.CommandID || current.CommandGeneration != task.CommandGeneration || !current.LeaseUntil.After(time.Now().UTC()) {
				return application.ErrImportConflict
			}
			if task.State == "BLOCKED" && task.RecipientHTTPStatus != 0 && current.RecipientHTTPStatus == 0 {
				digest, _ := contract.ImportDigest(task.RequestPayload)
				payload, e := json.Marshal(map[string]any{"item_id": item, "target": task.Target, "command_id": task.CommandID, "command_generation": task.CommandGeneration, "request_digest": digest, "http_status": task.RecipientHTTPStatus, "recipient_code": task.RecipientCode})
				if e != nil {
					return e
				}
				if e = insertAuditOutbox(tx, b.TenantID, newID(), AuditOutboxEntry{ActorID: "SYSTEM", Action: "CONTRACT_MANAGEMENT:IMPORT_TARGET_REJECTED", ResourceType: "contract", ResourceID: b.Items[i].ContractID, Payload: payload}); e != nil {
					return e
				}
			}
			task.Version = current.Version + 1
			*current = task
			updateImportTargetBatchState(b)
			return nil
		}
	}
	return application.ErrImportConflict
}

// updateImportTargetBatchState 按全部目标实际结果收敛批次状态。
// 参数：b为当前持锁批次。
// 返回值：无；任一BLOCKED保持PARTIAL，全目标完成后才COMPLETED。
func updateImportTargetBatchState(b *contract.ImportBatch) {
	b.State = "TARGETS_PENDING"
	complete := true
	for _, item := range b.Items {
		for _, target := range item.Targets {
			if target.State != "COMPLETE" && target.State != "NOT_APPLICABLE" {
				complete = false
			}
			if target.State == "BLOCKED" {
				b.State = "PARTIAL"
			}
		}
	}
	if complete {
		b.State = "COMPLETED"
	}
}

// ActivateImportResponsibility 仅在批准接续、项目完成与财务对账后受控激活来源责任。
// 参数：ctx为上下文，tenant/item/revision为冻结来源。
// 返回值：权威责任版本；不会把来源事件发送当作接收方ACK。
func (r *Repository) ActivateImportResponsibility(ctx context.Context, tenant, item string, revision uint64) (version uint64, err error) {
	b, e := r.GetImportItemBatch(ctx, tenant, item)
	if e != nil {
		return 0, e
	}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var batch importBatchRecord
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, b.ID).Take(&batch).Error; e != nil {
			return e
		}
		current, e := decodeImportBatch(batch)
		if e != nil {
			return e
		}
		if current.Confirmation == nil || current.Confirmation.State != "APPROVED" {
			return application.ErrImportConflict
		}
		for _, it := range current.Items {
			if it.ID != item {
				continue
			}
			if it.SourceRevision != revision {
				return application.ErrImportConflict
			}
			if it.Item.Mode != contract.ImportContinueLive || contract.ValidateHistoryEvidence(it.Item) != nil {
				return application.ErrImportUnsupported
			}
			for _, t := range it.Targets {
				if t.Target == "PROJECT" && t.State != "COMPLETE" {
					return application.ErrImportTargetWaiting
				}
			}
			var c contractRecord
			if e = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND id=?", tenant, it.ContractID).Take(&c).Error; e != nil {
				return e
			}
			if c.ImportConfirmedAt == nil || c.ImportSourceRevision != revision || c.ImportMode != "CONTINUE_LIVE" {
				return application.ErrImportConflict
			}
			if c.ImportResponsibilityActivatedAt == nil {
				now := time.Now().UTC()
				if e = tx.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, c.ID).Update("import_responsibility_activated_at", now).Error; e != nil {
					return e
				}
			}
			if e = enqueueContractResponsibility(tx, tenant, c.ID); e != nil {
				return e
			}
			var stream contractResponsibilityStream
			if e = tx.Where("tenant_id=? AND contract_id=?", tenant, c.ID).Take(&stream).Error; e != nil {
				return e
			}
			version = stream.Version
			return nil
		}
		return application.ErrImportConflict
	})
	return
}

// GetImportTargetContract 读取出站所需权威字段，避免每次水位轮询加载原件BLOB。
// 参数：ctx为上下文，tenant/id限定已获准任务的合同身份。
// 返回值：含原分期和服务映射的合同元数据，原件字节始终为空。
func (r *Repository) GetImportTargetContract(ctx context.Context, tenant, id string) (contract.Contract, error) {
	var row contractRecord
	e := r.db.WithContext(ctx).Omit("rendered_document", "content", "template_values_json").Where("tenant_id=? AND id=?", tenant, id).Take(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return contract.Contract{}, apperrors.ErrNotFound
	}
	if e != nil {
		return contract.Contract{}, e
	}
	rows, e := r.attachPaymentTerms(ctx, tenant, []contract.Contract{contractFromRecord(row)})
	if e != nil {
		return contract.Contract{}, e
	}
	return rows[0], nil
}
