package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) UpdateContractDraft(ctx context.Context, c contract.Contract, expectedVersion uint64, actorUserID string) error {
	values, err := json.Marshal(c.TemplateValues)
	if err != nil {
		return err
	}
	systems, err := json.Marshal(c.Systems)
	if err != nil {
		return err
	}
	items, err := json.Marshal(c.ServiceItems)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current contractRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", c.TenantID, c.ID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return apperrors.ErrVersionConflict
		}
		if contract.Status(current.Status) != contract.StatusDraft {
			return apperrors.ErrStateConflict
		}
		if c.PaymentTerms == nil {
			stored, err := (&Repository{db: tx}).ListContractPaymentTerms(ctx, c.TenantID, c.ID)
			if err != nil {
				return err
			}
			c.PaymentTerms = stored
			if len(stored) > 0 && !strings.EqualFold(strings.TrimSpace(c.Currency), strings.TrimSpace(current.Currency)) {
				return application.ErrValidation
			}
		}
		if _, err := contract.NormalizePaymentTerms(c.PaymentTerms, c.AmountMinor, c.Currency); err != nil {
			return fmt.Errorf("%w: %v", application.ErrValidation, err)
		}
		now := time.Now().UTC()
		// 仅更新可编辑的草稿内容。调用方不能修改身份、负责人、源绑定、
		// 合同编号或原始创建审计。
		result := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ? AND version = ? AND status = ?", c.TenantID, c.ID, expectedVersion, contract.StatusDraft).Updates(map[string]any{
			"title": c.Title, "contract_type": c.Type, "service_type": c.ServiceType, "customer_credit_level": stringPtr(c.CustomerCreditLevel),
			"opportunity_id": stringPtr(c.OpportunityID), "opportunity_name": stringPtr(c.OpportunityName), "crm_customer_id": uintPtr(c.CRMCustomerID),
			"customer_name": stringPtr(c.CustomerName), "customer_address": stringPtr(c.CustomerAddress), "customer_contact": stringPtr(c.CustomerContact), "customer_phone": stringPtr(c.CustomerPhone),
			"systems_json": systems, "service_items_json": items, "amount_minor": c.AmountMinor, "currency": c.Currency,
			"content": c.Content, "template_values_json": values, "rendered_document": c.Document, "content_hash": c.ContentHash,
			"start_date": c.StartDate, "end_date": c.EndDate, "version": expectedVersion + 1, "updated_at": now, "updated_by": actorUserID,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrVersionConflict
		}
		if err := replaceContractPaymentTerms(tx, c); err != nil {
			return err
		}
		if err := tx.Create(&lifecycleEventRecord{ID: newID(), TenantID: c.TenantID, ContractID: c.ID, FromStatus: string(contract.StatusDraft), ToStatus: string(contract.StatusDraft), ActorUserID: stringPtr(actorUserID), Reason: stringPtr("contract draft edited"), IdempotencyKey: fmt.Sprintf("%s:draft:v%d", c.ID, expectedVersion+1), OccurredAt: now}).Error; err != nil {
			return err
		}
		// 草稿修改同样是关键业务变更：与业务行同事务写审计，记录版本跃迁。
		// 不记录合同正文、模板值或客户联系方式。
		auditPayload, err := json.Marshal(map[string]any{
			"contract_id": c.ID, "from_version": expectedVersion, "to_version": expectedVersion + 1,
			"status": string(contract.StatusDraft), "amount_minor": c.AmountMinor, "currency": c.Currency,
		})
		if err != nil {
			return err
		}
		meta := auditctx.FromContext(ctx)
		return insertAuditOutbox(tx, c.TenantID, newID(), AuditOutboxEntry{
			ActorID: actorUserID, Action: "CONTRACT_MANAGEMENT:UPDATED",
			ResourceType: "contract", ResourceID: c.ID, Payload: auditPayload,
			LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
		})
	})
}

// RecordDocumentAccess 在敏感文档返回前写审计 outbox（统一审计方案 §3.2）。
// 只记录合同、文档类型与操作者，不记录文件名以外的内容，也不缓存文档字节。
func (r *Repository) RecordDocumentAccess(ctx context.Context, tenantID, contractID, documentKind, actorID string) error {
	payload, err := json.Marshal(map[string]any{"contract_id": contractID, "document_kind": documentKind})
	if err != nil {
		return err
	}
	meta := auditctx.FromContext(ctx)
	actor := actorID
	if actor == "" {
		actor = "UNKNOWN"
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
			ActorID: actor, Action: "CONTRACT_MANAGEMENT:DOCUMENT_DOWNLOADED",
			ResourceType: "contract", ResourceID: contractID, Payload: payload,
			LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
		})
	})
}
