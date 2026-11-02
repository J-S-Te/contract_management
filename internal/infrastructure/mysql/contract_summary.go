package mysql

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"gorm.io/gorm"
)

// FindContractSummaryCandidate 按当前租户和 contract_ref 查找合同摘要候选。
// contract_ref 先按稳定合同 ID 精确匹配，未命中再按合同编号匹配；
// 两条路径都强制携带 tenant_id，任何请求参数都不能跨租户。
// 参数：tenantID 必须来自已验签机器令牌，contractRef 为合同 ID 或编号。
// 返回值：候选行；合同不存在返回 apperrors.ErrNotFound。
func (r *Repository) FindContractSummaryCandidate(ctx context.Context, tenantID, contractRef string) (application.ContractSummaryCandidate, error) {
	var row contractRecord
	// 先按稳定 ID 精确匹配：CRM 传入 ULID 时不该被同租户的编号撞库影响。
	err := r.db.WithContext(ctx).
		Select("id", "contract_number", "crm_customer_id", "opportunity_id", "updated_at").
		Where("tenant_id = ? AND id = ?", tenantID, contractRef).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = r.db.WithContext(ctx).
			Select("id", "contract_number", "crm_customer_id", "opportunity_id", "updated_at").
			Where("tenant_id = ? AND contract_number = ?", tenantID, contractRef).
			Take(&row).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return application.ContractSummaryCandidate{}, apperrors.ErrNotFound
	}
	if err != nil {
		return application.ContractSummaryCandidate{}, err
	}
	candidate := application.ContractSummaryCandidate{
		ContractID:       row.ID,
		ContractNumber:   stringValue(row.ContractNumber),
		CRMCustomerID:    uint64Value(row.CRMCustomerID),
		CRMOpportunityID: uint64FromString(stringValue(row.OpportunityID)),
		UpdatedAt:        row.UpdatedAt,
	}
	latest, earliest, source, err := r.contractSummaryLinkedAt(ctx, tenantID, row.ID)
	if err != nil {
		return application.ContractSummaryCandidate{}, err
	}
	candidate.LinkedAt = latest
	candidate.FirstLinkedAt = earliest
	candidate.LinkedAtSource = source
	return candidate, nil
}

// contractSummaryLinkedAt 取合同与商机关联的可证明时间及其证据来源。
// 参数：ctx 控制查询，tenantID 为可信租户（来自已验签令牌），contractID 为合同稳定 ID。
// 返回值：latest 为“最新一次”可证明关联时间，earliest 为同一证据层内“最早一次”
// 可证明关联时间，source 为证据来源枚举；全部证据缺失时三者均为零值，
// 由应用层回退合同 updated_at（近似）并标注 APPROX_CONTRACT_UPDATED。
//
// 证据优先级固定（都只读、都强制带 tenant_id，命中一层即返回，不跨层混合）：
//  1. CRM_REVIEW：商机接入审核通过（LINK_CONFIRMED）行上的 reviewed_at ——
//     审核动作本身证明“已被 CRM 客户认领”；
//  2. REVIEW_LOG：历史接入行没有回填 reviewed_at 时，取同接入单 LINK_CONFIRMED
//     审核流水的 created_at，语义与 reviewed_at 一致；
//  3. AUDIT_LOG：关联确认与业务状态变更同事务写入的审计流水
//     con_audit_outbox（resource_type='contract'、resource_id=合同 ID、
//     action='CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED'、result='SUCCESS'）
//     的 created_at —— 即使接入单被清理或迁移，这条权威痕迹仍在；
//  4. INTAKE_CREATED：以上都没有时退回接入单自身的 created_at；它早于确认动作，
//     只是“关联请求已存在”的下限，因此精度为 approx。
//
// 每层的 latest/earliest 都只在本层内取：多张接入单或多次关联确认时，
// linked_at 取最新、first_linked_at 取最早，来源与精度始终同源。
func (r *Repository) contractSummaryLinkedAt(ctx context.Context, tenantID, contractID string) (time.Time, time.Time, string, error) {
	type linkedIntake struct {
		IntakeID   string
		ReviewedAt *time.Time
		CreatedAt  time.Time
	}
	var intakes []linkedIntake
	if err := r.db.WithContext(ctx).Model(&opportunityIntakeRecord{}).
		Select("intake_id, reviewed_at, created_at").
		Where("tenant_id = ? AND contract_id = ? AND status = ?", tenantID, contractID, application.OpportunityIntakeLinkConfirmed).
		Scan(&intakes).Error; err != nil {
		return time.Time{}, time.Time{}, "", err
	}
	// 第 1 层：接入单上的审核通过时间。
	var latest, earliest time.Time
	intakeIDs := make([]string, 0, len(intakes))
	for _, intake := range intakes {
		intakeIDs = append(intakeIDs, intake.IntakeID)
		if intake.ReviewedAt == nil || intake.ReviewedAt.IsZero() {
			continue
		}
		latest = laterTime(latest, *intake.ReviewedAt)
		earliest = earlierTime(earliest, *intake.ReviewedAt)
	}
	if !latest.IsZero() {
		return latest, earliest, application.ContractSummaryLinkSourceCRMReview, nil
	}
	// 第 2 层：同接入单的审核流水（接入行没有 reviewed_at 的历史数据）。
	if len(intakeIDs) > 0 {
		layerLatest, layerEarliest, found, err := r.contractSummaryAuditBounds(ctx, &opportunityIntakeReviewRecord{},
			"tenant_id = ? AND intake_id IN ? AND decision = ?", tenantID, intakeIDs, application.OpportunityIntakeLinkConfirmed)
		if err != nil {
			return time.Time{}, time.Time{}, "", err
		}
		if found {
			return layerLatest, layerEarliest, application.ContractSummaryLinkSourceReviewLog, nil
		}
	}
	// 第 3 层：关联确认动作写下的权威审计流水（不依赖接入单是否仍存在）。
	layerLatest, layerEarliest, found, err := r.contractSummaryAuditBounds(ctx, &auditOutboxRecord{},
		"tenant_id = ? AND resource_type = ? AND resource_id = ? AND action = ? AND result = ?",
		tenantID, "contract", contractID, "CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED", "SUCCESS")
	if err != nil {
		return time.Time{}, time.Time{}, "", err
	}
	if found {
		return layerLatest, layerEarliest, application.ContractSummaryLinkSourceAuditLog, nil
	}
	// 第 4 层：接入单创建时间（下限近似，不是确认时刻本身）。
	for _, intake := range intakes {
		if intake.CreatedAt.IsZero() {
			continue
		}
		latest = laterTime(latest, intake.CreatedAt)
		earliest = earlierTime(earliest, intake.CreatedAt)
	}
	if !latest.IsZero() {
		return latest, earliest, application.ContractSummaryLinkSourceIntakeCreated, nil
	}
	return time.Time{}, time.Time{}, "", nil
}

// contractSummaryAuditBounds 在给定只读条件内取 created_at 的最早与最晚值。
// 参数：ctx 控制查询，model 为目标表模型，query/args 为已包含 tenant_id 的条件。
// 返回值：latest/earliest 为该层证据的时间边界，found 表示该层是否有证据行。
func (r *Repository) contractSummaryAuditBounds(ctx context.Context, model any, query string, args ...any) (time.Time, time.Time, bool, error) {
	var bounds struct {
		Earliest *time.Time `gorm:"column:earliest_at"`
		Latest   *time.Time `gorm:"column:latest_at"`
	}
	if err := r.db.WithContext(ctx).Model(model).
		Select("MIN(created_at) AS earliest_at, MAX(created_at) AS latest_at").
		Where(query, args...).Scan(&bounds).Error; err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if bounds.Latest == nil || bounds.Earliest == nil || bounds.Latest.IsZero() {
		return time.Time{}, time.Time{}, false, nil
	}
	return *bounds.Latest, *bounds.Earliest, true, nil
}

// laterTime 返回两个时间中较晚者（零值视为“尚无证据”）。
func laterTime(current, candidate time.Time) time.Time {
	if candidate.After(current) {
		return candidate
	}
	return current
}

// earlierTime 返回两个时间中较早者（零值视为“尚无证据”）。
func earlierTime(current, candidate time.Time) time.Time {
	if current.IsZero() || candidate.Before(current) {
		return candidate
	}
	return current
}

// uint64Value 把可空列转换为零值安全的结果。
func uint64Value(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

// uint64FromString 解析合同行上的商机号（历史列为字符串）。
func uint64FromString(value string) uint64 {
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}
