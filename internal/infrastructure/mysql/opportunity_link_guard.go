package mysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// confirmOpportunityContractLink 在审核事务内执行商机核对关联。
// 参数：tx 为审核事务，tenantID 为可信租户，intake 为已锁定的接入记录，
// reviewer/display 为审核操作者。
// 返回值：关联后的合同稳定 ID 与编号；来源、状态或客户归属不允许时返回
// application.ErrOpportunityLinkRejected，合同不存在返回 apperrors.ErrNotFound。
// 该函数只改合同 CRM 引用并在同事务写审计，不伪造任何下游投递成功。
func confirmOpportunityContractLink(tx *gorm.DB, tenantID string, intake opportunityIntakeRecord, reviewer, display string) (string, string, error) {
	ref := strings.TrimSpace(intake.ContractRef)
	if ref == "" {
		return "", "", apperrors.ErrNotFound
	}
	// 先按稳定 ID 精确查，再按编号查；同时命中不同合同时拒绝，
	// 避免一个 ref 在两列之间“就近匹配”到另一份合同。
	var byID, byNumber contractRecord
	idErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND id = ?", tenantID, ref).Take(&byID).Error
	if idErr != nil && !errors.Is(idErr, gorm.ErrRecordNotFound) {
		return "", "", idErr
	}
	numberErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tenant_id = ? AND contract_number = ?", tenantID, ref).Take(&byNumber).Error
	if numberErr != nil && !errors.Is(numberErr, gorm.ErrRecordNotFound) {
		return "", "", numberErr
	}
	var idMatch, numberMatch *contractRecord
	if idErr == nil {
		idMatch = &byID
	}
	if numberErr == nil {
		numberMatch = &byNumber
	}
	target, err := resolveOpportunityLinkTarget(idMatch, numberMatch)
	if err != nil {
		return "", "", err
	}
	if err := validateOpportunityLinkTarget(target, intake); err != nil {
		return "", "", err
	}
	beforeOpportunity := stringValue(target.OpportunityID)
	beforeCustomer := uint64Value(target.CRMCustomerID)
	now := time.Now().UTC()
	updated := tx.Model(&contractRecord{}).
		Where("tenant_id = ? AND id = ?", tenantID, target.ID).
		Updates(map[string]any{
			"opportunity_id":  strconv.FormatUint(intake.OpportunityID, 10),
			"crm_customer_id": intake.CustomerID,
			"updated_at":      now,
			"updated_by":      reviewer,
		})
	if updated.Error != nil {
		return "", "", updated.Error
	}
	if updated.RowsAffected != 1 {
		return "", "", apperrors.ErrNotFound
	}
	auditPayload, err := json.Marshal(map[string]any{
		"intake_id":             intake.IntakeID,
		"event_id":              intake.EventID,
		"operator":              reviewer,
		"operator_display_name": display,
		"before": map[string]any{
			"opportunity_id":  beforeOpportunity,
			"crm_customer_id": beforeCustomer,
		},
		"after": map[string]any{
			"opportunity_id":  strconv.FormatUint(intake.OpportunityID, 10),
			"crm_customer_id": intake.CustomerID,
		},
	})
	if err != nil {
		return "", "", err
	}
	meta := auditctx.FromContext(tx.Statement.Context)
	if err := insertAuditOutbox(tx, tenantID, newID(), AuditOutboxEntry{
		ActorID: reviewer, Action: "CONTRACT_MANAGEMENT:OPPORTUNITY_LINK_CONFIRMED",
		ResourceType: "contract", ResourceID: target.ID, Payload: auditPayload,
		LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
	}); err != nil {
		return "", "", err
	}
	number := stringValue(target.ContractNumber)
	if number == "" {
		number = ref
	}
	return target.ID, number, nil
}

// resolveOpportunityLinkTarget 决定 id/编号两种匹配指向的合同。
// 参数：idMatch/numberMatch 为可空的命中行。
// 返回值：唯一目标；两者指向不同合同时返回拒绝错误，都不存在返回未找到。
func resolveOpportunityLinkTarget(idMatch, numberMatch *contractRecord) (contractRecord, error) {
	if idMatch != nil && numberMatch != nil && idMatch.ID != numberMatch.ID {
		return contractRecord{}, fmt.Errorf("%w: 合同编号与合同 ID 指向不同合同", application.ErrOpportunityLinkRejected)
	}
	if idMatch != nil {
		return *idMatch, nil
	}
	if numberMatch != nil {
		return *numberMatch, nil
	}
	return contractRecord{}, apperrors.ErrNotFound
}

// validateOpportunityLinkTarget 校验商机核对的来源、状态、客户归属与已关联商机。
// 参数：target 为权威合同行，intake 为接入记录。
// 返回值：任一不允许条件返回 application.ErrOpportunityLinkRejected；
// 这是 409 业务冲突，不是“未找到”，便于运营看到“需走受控更正”的明确提示。
func validateOpportunityLinkTarget(target contractRecord, intake opportunityIntakeRecord) error {
	if intake.CustomerID == 0 {
		return fmt.Errorf("%w: 商机接入缺少 CRM 客户，不能建立合同客户归属", application.ErrOpportunityLinkRejected)
	}
	// 历史导入基线禁止被普通商机覆盖：它们的客户/金额/责任已冻结。
	if !ordinaryContractRecord(target) {
		return fmt.Errorf("%w: 历史导入合同不能通过普通商机核对改写", application.ErrOpportunityLinkRejected)
	}
	switch contract.Status(target.Status) {
	case contract.StatusArchived, contract.StatusTerminated:
		return fmt.Errorf("%w: 已归档或已终止合同不能建立商机关联", application.ErrOpportunityLinkRejected)
	}
	if target.CounterpartySource == contract.CounterpartyLocal || target.CategorySource == contract.CategoryLocal {
		return fmt.Errorf("%w: 本地来源合同不能自动改绑CRM商机", application.ErrOpportunityLinkRejected)
	}
	existingCustomer := uint64Value(target.CRMCustomerID)
	if existingCustomer != 0 {
		if existingCustomer != intake.CustomerID {
			return fmt.Errorf("%w: 合同已归属其他 CRM 客户，需走受控更正", application.ErrOpportunityLinkRejected)
		}
		// 同客户不等于同一商机：合同已关联商机 A 时改写为同客户的商机 B，
		// 会静默改写 CRM 商机计数与数析事实且不重发下游事件，必须走受控更正。
		// 已关联同一商机（含重复确认）保持幂等放行。
		if linked := strings.TrimSpace(stringValue(target.OpportunityID)); linked != "" &&
			linked != strconv.FormatUint(intake.OpportunityID, 10) {
			return fmt.Errorf("%w: 合同已关联其他商机，需走受控更正", application.ErrOpportunityLinkRejected)
		}
		return nil
	}
	// 合同没有 CRM 客户时，只有 draft/pending 还没产生下游事实，可以安全补建关联。
	// 已审批/生效合同补客户会改写已投递的结算/项目事实，必须走受控更正与重发流程。
	if contract.Status(target.Status).ApprovalPassed() {
		return fmt.Errorf("%w: 已审批或生效合同需要补建客户归属，请走受控更正", application.ErrOpportunityLinkRejected)
	}
	return nil
}
