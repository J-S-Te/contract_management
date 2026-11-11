package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"gorm.io/gorm"
)

type pricingRevisionRecord struct {
	ApprovalID      string `gorm:"primaryKey"`
	TenantID        string
	ContractID      string
	ExpectedVersion uint64
	ProposalJSON    []byte `gorm:"column:proposal_json;type:json"`
	CreatedAt       time.Time
}

// TableName 指定定价修订审批的不可变申请表。
// 参数：无。
// 返回值：权威表名。
func (pricingRevisionRecord) TableName() string { return "con_pricing_revision" }

// AcceptPricingRevision 将冻结修订与原审批受理、快照和启动纳入同一事务。
// 参数：ctx/in为审批边界，candidate为候选完整合同，start为有界Temporal启动。
// 返回值：任何失败整体回滚；成功后保留待审申请，当前合同不变。
func (r *Repository) AcceptPricingRevision(ctx context.Context, in workflows.StartApprovalActivityInput, candidate contract.Contract, start func(context.Context) (string, error)) error {
	if candidate.ID != in.ContractID || candidate.TenantID != in.TenantID || candidate.Version != in.ExpectedVersion {
		return application.ErrValidation
	}
	if _, err := contract.PricingTotal(candidate); err != nil {
		return application.ErrValidation
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		data, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		if err := tx.Create(&pricingRevisionRecord{ApprovalID: in.ApprovalID, TenantID: in.TenantID, ContractID: in.ContractID, ExpectedVersion: in.ExpectedVersion, ProposalJSON: data, CreatedAt: time.Now().UTC()}).Error; err != nil {
			return err
		}
		if err := (&Repository{db: tx}).AcceptStatusChange(ctx, in, start); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"approval_id": in.ApprovalID, "contract_id": candidate.ID, "from_version": candidate.Version, "basis_date": candidate.Pricing.BasisDate, "reason": candidate.Pricing.Reason})
		return insertAuditOutbox(tx, in.TenantID, newID(), AuditOutboxEntry{ActorID: in.ApplicantUserID, Action: "CONTRACT_MANAGEMENT:PRICING_REVISION_SUBMITTED", ResourceType: "contract", ResourceID: candidate.ID, Payload: payload})
	})
}

// pricingProposal 读取审批绑定的冻结定价修订。
// 参数：tx为审批事务，tenant/id为可信审批边界。
// 返回值：候选、是否存在及结构错误。
func pricingProposal(tx *gorm.DB, tenant, id string) (contract.Contract, bool, error) {
	var row pricingRevisionRecord
	var c contract.Contract
	err := tx.Where("tenant_id=? AND approval_id=?", tenant, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	if err := json.Unmarshal(row.ProposalJSON, &c); err != nil {
		return c, false, err
	}
	return c, true, nil
}

// applyPricingRevision 在原审批终决事务中原子替换已独立批准的金额与冻结价。
// 参数：tx为事务，in为终决，current为已锁定当前合同行。
// 返回值：非定价审批返回false；已受理修订必须版本一致，失败整体回滚。
func applyPricingRevision(tx *gorm.DB, in workflows.CompleteApprovalActivityInput, current contractRecord, applicant string) (bool, error) {
	c, found, err := pricingProposal(tx, in.TenantID, in.ApprovalID)
	if err != nil || !found {
		return found, err
	}
	if in.Status != approval.StatusApproved {
		return true, nil
	}
	if in.ActorUserID == "" || in.ActorUserID == applicant {
		return true, application.ErrForbidden
	}
	if c.Version != current.Version || c.ID != current.ID || c.TenantID != in.TenantID || !application.FinancialChangeStatusEligible(contract.Status(current.Status)) {
		return true, apperrors.ErrVersionConflict
	}
	locked := contractFromRecord(current)
	services, err := pricingRevisionServiceIdentities(current.ID, locked.ServiceItems, c.ServiceItems)
	if err != nil {
		return true, err
	}
	locked.ServiceItems = services
	locked.AmountMinor = c.AmountMinor
	locked.Pricing = c.Pricing
	if c.Currency != current.Currency {
		return true, application.ErrFinancialChangeCurrencyLocked
	}
	if _, err := contract.PricingTotal(locked); err != nil {
		return true, err
	}
	servicesJSON, err := json.Marshal(services)
	if err != nil {
		return true, err
	}
	now := time.Now().UTC()
	result := tx.Model(&contractRecord{}).Where("tenant_id=? AND id=? AND version=?", in.TenantID, c.ID, c.Version).Updates(map[string]any{"amount_minor": c.AmountMinor, "service_items_json": servicesJSON, "pricing_json": pricingJSON(c.Pricing), "version": c.Version + 1, "updated_at": now, "updated_by": in.ActorUserID})
	if result.Error != nil {
		return true, result.Error
	}
	if result.RowsAffected != 1 {
		return true, apperrors.ErrVersionConflict
	}
	if err := replaceContractPaymentTerms(tx, c); err != nil {
		return true, err
	}
	outcome, err := enqueueFinancialEffective(tx, in.TenantID, c.ID)
	if err != nil {
		return true, err
	}
	if !outcome.Enqueued {
		return true, application.ErrFinancialChangeEventNotQueued
	}
	if err := tx.Create(&lifecycleEventRecord{ID: newID(), TenantID: in.TenantID, ContractID: c.ID, FromStatus: current.Status, ToStatus: current.Status, ActorUserID: stringPtr(in.ActorUserID), Reason: stringPtr(c.Pricing.Reason), IdempotencyKey: in.ApprovalID + ":pricing-applied", OccurredAt: now}).Error; err != nil {
		return true, err
	}
	data, _ := json.Marshal(map[string]any{"approval_id": in.ApprovalID, "version": c.Version + 1, "basis_date": c.Pricing.BasisDate, "reason": c.Pricing.Reason})
	return true, insertAuditOutbox(tx, in.TenantID, newID(), AuditOutboxEntry{ActorID: in.ActorUserID, Action: "CONTRACT_MANAGEMENT:PRICING_REVISION_APPROVED", ResourceType: "contract", ResourceID: c.ID, Payload: data})
}

// pricingRevisionServiceIdentities 仅为原空身份服务补齐候选稳定身份，拒绝趁审批替换业务内容。
// 参数：contractID为权威合同身份，current为已锁定服务项，proposed为审批候选服务项。
// 返回值：内容和顺序完全一致的可信服务项；既有身份漂移或内容变化返回冲突。
func pricingRevisionServiceIdentities(contractID string, current, proposed []contract.ServiceItem) ([]contract.ServiceItem, error) {
	if len(current) != len(proposed) {
		return nil, apperrors.ErrStateConflict
	}
	out := append([]contract.ServiceItem(nil), current...)
	for i := range out {
		if proposed[i].SourceID == "" || out[i].SourceID != "" && out[i].SourceID != proposed[i].SourceID {
			return nil, apperrors.ErrStateConflict
		}
		if out[i].SourceID == "" && proposed[i].SourceID != contract.ProjectServiceBaseID(contractID, i, out[i]) {
			return nil, fmt.Errorf("%w: 定价候选服务身份与既有项目引用不一致，须撤回后重新提交", apperrors.ErrStateConflict)
		}
		out[i].SourceID = proposed[i].SourceID
		oldJSON, err := json.Marshal(out[i])
		if err != nil {
			return nil, err
		}
		newJSON, err := json.Marshal(proposed[i])
		if err != nil {
			return nil, err
		}
		if string(oldJSON) != string(newJSON) {
			return nil, apperrors.ErrStateConflict
		}
	}
	return out, nil
}
