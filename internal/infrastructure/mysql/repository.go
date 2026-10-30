package mysql

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	crmintegration "github.com/j-s-te/contract-management/internal/integration/crm"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (r *Repository) AcceptOpportunityIntake(ctx context.Context, in application.OpportunityIntake) (application.OpportunityIntake, error) {
	var row opportunityIntakeRecord
	err := r.db.WithContext(ctx).Where("tenant_id=? AND event_id=?", in.TenantID, in.EventID).First(&row).Error
	if err == nil {
		return opportunityIntakeFromRecord(row), nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return application.OpportunityIntake{}, err
	}
	now := time.Now().UTC()
	row = opportunityIntakeRecord{IntakeID: in.IntakeID, TenantID: in.TenantID, EventID: in.EventID, OpportunityID: in.OpportunityID, EventVersion: in.EventVersion, OpportunityNo: in.OpportunityNo, CustomerID: in.CustomerID, ContractRef: in.ContractRef, ExpectedAmount: in.ExpectedAmount, OccurredAt: in.OccurredAt, Status: application.OpportunityIntakeAccepted, Version: 1, CreatedAt: now, UpdatedAt: now}
	if in.SourceRequestID != "" {
		row.SourceRequestID = &in.SourceRequestID
	}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			if e := r.db.WithContext(ctx).Where("tenant_id=? AND event_id=?", in.TenantID, in.EventID).First(&row).Error; e == nil {
				return opportunityIntakeFromRecord(row), nil
			}
		}
		return application.OpportunityIntake{}, err
	}
	return opportunityIntakeFromRecord(row), nil
}

func (r *Repository) ListOpportunityIntakes(ctx context.Context, tenant, status, cursor string, limit int) (application.OpportunityIntakePage, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := r.db.WithContext(ctx).Where("tenant_id=?", tenant)
	if status != "" {
		q = q.Where("status=?", status)
	}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return application.OpportunityIntakePage{}, application.ErrValidation
		}
		parts := strings.Split(string(raw), "|")
		if len(parts) != 2 {
			return application.OpportunityIntakePage{}, application.ErrValidation
		}
		q = q.Where("(created_at < ? OR (created_at = ? AND intake_id < ?))", parts[0], parts[0], parts[1])
	}
	var rows []opportunityIntakeRecord
	if err := q.Order("created_at DESC, intake_id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return application.OpportunityIntakePage{}, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]application.OpportunityIntake, 0, len(rows))
	for _, row := range rows {
		out = append(out, opportunityIntakeFromRecord(row))
	}
	next := ""
	if more && len(rows) > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(rows[len(rows)-1].CreatedAt.UTC().Format("2006-01-02 15:04:05.000") + "|" + rows[len(rows)-1].IntakeID))
	}
	return application.OpportunityIntakePage{Items: out, PageSize: len(out), NextCursor: next, HasMore: more}, nil
}

func (r *Repository) GetOpportunityIntake(ctx context.Context, tenant, id string) (application.OpportunityIntake, error) {
	var row opportunityIntakeRecord
	e := r.db.WithContext(ctx).Where("tenant_id=? AND intake_id=?", tenant, id).First(&row).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		return application.OpportunityIntake{}, apperrors.ErrNotFound
	}
	if e != nil {
		return application.OpportunityIntake{}, e
	}
	return opportunityIntakeFromRecord(row), nil
}

func (r *Repository) ReviewOpportunityIntake(ctx context.Context, tenant, id, reviewer string, review application.OpportunityIntakeReview, key, hash, display string) (application.OpportunityIntake, error) {
	var out application.OpportunityIntake
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row opportunityIntakeRecord
		// 锁住接入记录，防止不同幂等键的并发审核同时越过 ACCEPTED 状态校验。
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id=? AND intake_id=?", tenant, id).First(&row).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return apperrors.ErrNotFound
			}
			return e
		}
		var prior opportunityIntakeReviewRecord
		if e := tx.Where("tenant_id=? AND intake_id=? AND idempotency_key=?", tenant, id, key).First(&prior).Error; e == nil {
			if prior.RequestHash != hash {
				return apperrors.ErrStateConflict
			}
			return json.Unmarshal(prior.ResponseJSON, &out)
		} else if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		if row.Version != review.Version {
			return apperrors.ErrVersionConflict
		}
		if row.Status != application.OpportunityIntakeAccepted {
			return apperrors.ErrStateConflict
		}
		now := time.Now().UTC()
		row.Status = review.Decision
		row.Version++
		row.ReviewedBy = &reviewer
		row.ReviewerDisplayName = &display
		row.ReviewedAt = &now
		row.ReviewReason = &review.Reason
		row.UpdatedAt = now
		if review.Decision == application.OpportunityIntakeLinkConfirmed {
			var linked contractRecord
			if e := tx.Where("tenant_id=? AND (id=? OR contract_number=?)", tenant, reviewContractRef(row), row.ContractRef).First(&linked).Error; e != nil {
				return apperrors.ErrNotFound
			}
			updated := tx.Model(&contractRecord{}).Where("tenant_id=? AND id=?", tenant, linked.ID).Updates(map[string]any{"opportunity_id": strconv.FormatUint(row.OpportunityID, 10), "crm_customer_id": row.CustomerID})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected == 0 {
				return apperrors.ErrNotFound
			}
			out.ContractID = linked.ID
			out.ContractNumber = stringValue(linked.ContractNumber)
			row.ContractID = &linked.ID
			if linked.ContractNumber != nil {
				row.ContractNumber = linked.ContractNumber
			}
		}
		if e := tx.Save(&row).Error; e != nil {
			return e
		}
		linkedID, linkedNumber := out.ContractID, out.ContractNumber
		out = opportunityIntakeFromRecord(row)
		out.ContractID, out.ContractNumber = linkedID, linkedNumber
		data, marshalErr := json.Marshal(out)
		if marshalErr != nil {
			return marshalErr
		}
		callbackPayload, marshalErr := crmintegration.EncodeOpportunityLinkCallback(out)
		if marshalErr != nil {
			return marshalErr
		}
		// 确认与异常都必须回写 CRM，避免两边长期停留在不一致状态。
		if e := enqueueOpportunityLink(tx, out, callbackPayload); e != nil {
			return e
		}
		rec := opportunityIntakeReviewRecord{ReviewID: ulid.Make().String(), TenantID: tenant, IntakeID: id, IdempotencyKey: key, RequestHash: hash, Decision: review.Decision, Reason: review.Reason, Version: review.Version, ReviewerID: reviewer, ReviewerDisplayName: &display, ResponseJSON: data, CreatedAt: now}
		return tx.Create(&rec).Error
	})
	return out, err
}
func reviewContractRef(row opportunityIntakeRecord) string { return row.ContractRef }

type Repository struct{ db *gorm.DB }

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

func (r *Repository) GetContract(ctx context.Context, tenantID, id string) (contract.Contract, error) {
	var record contractRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.Contract{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.Contract{}, err
	}
	items, err := r.attachPaymentTerms(ctx, tenantID, []contract.Contract{contractFromRecord(record)})
	if err != nil {
		return contract.Contract{}, err
	}
	return items[0], nil
}

func (r *Repository) GetContractScoped(ctx context.Context, filter contract.ScopeFilter, id string) (contract.Contract, error) {
	var record contractRecord
	query := applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter).Where("id = ?", id)
	err := query.Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.Contract{}, apperrors.ErrNotFound
	}
	if err != nil {
		return contract.Contract{}, err
	}
	items, err := r.attachPaymentTerms(ctx, filter.TenantID, []contract.Contract{contractFromRecord(record)})
	if err != nil {
		return contract.Contract{}, err
	}
	return items[0], nil
}

func (r *Repository) ListContractsScoped(ctx context.Context, filter contract.ScopeFilter, keyword, status string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter)
	if keyword != "" {
		pattern := "%" + keyword + "%"
		query = query.Where("(contract_number LIKE ? OR title LIKE ? OR customer_name LIKE ?)", pattern, pattern, pattern)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var records []contractRecord
	if err := query.Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, filter.TenantID, result)
}

func applyContractScope(query *gorm.DB, filter contract.ScopeFilter) *gorm.DB {
	query = query.Where("tenant_id = ?", filter.TenantID)
	if filter.AllowAll {
		return query
	}
	var predicates []string
	var values []any
	if filter.AllowSelf && filter.IdentityID != "" {
		predicates = append(predicates, "owner_identity_id = ?")
		values = append(values, filter.IdentityID)
	}
	if len(filter.OrganizationIDs) > 0 {
		predicates = append(predicates, "owner_org_id IN ?")
		values = append(values, filter.OrganizationIDs)
	}
	if len(filter.ProjectIDs) > 0 {
		predicates = append(predicates, "project_id IN ?")
		values = append(values, filter.ProjectIDs)
	}
	if len(predicates) == 0 {
		return query.Where("1 = 0")
	}
	return query.Where("("+strings.Join(predicates, " OR ")+")", values...)
}

func (r *Repository) ListContracts(ctx context.Context, tenantID, keyword, ownerUserID, status string, limit int) ([]contract.Contract, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := r.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if keyword != "" {
		pattern := "%" + keyword + "%"
		query = query.Where("(contract_number LIKE ? OR title LIKE ? OR customer_name LIKE ?)", pattern, pattern, pattern)
	}
	if ownerUserID != "" {
		query = query.Where("owner_user_id = ?", ownerUserID)
	}
	if status != "" {
		query = query.Where("status = ?", status)
	}
	var records []contractRecord
	if err := query.Omit("rendered_document", "template_values_json").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]contract.Contract, 0, len(records))
	for _, record := range records {
		result = append(result, contractFromRecord(record))
	}
	return r.attachPaymentTerms(ctx, tenantID, result)
}

func dashboardActiveStatuses() []contract.Status {
	return []contract.Status{contract.StatusActive, contract.StatusInProgress, contract.StatusPendingPay}
}

// 已归档合同仍是报表中的有效业务合同。它们处于
// 终态，结束日期可能已过，因此须与
// 当前有效状态分开计数，绝不能归类为过期。
func dashboardActiveScope(db *gorm.DB, activeStatuses []contract.Status, today time.Time) *gorm.DB {
	return db.Where("(status = ? OR (status IN ? AND (end_date IS NULL OR end_date >= ?)))", contract.StatusArchived, activeStatuses, today)
}

func dashboardStatusIsActive(status contract.Status, endDate *time.Time, today time.Time) bool {
	if status == contract.StatusArchived {
		return true
	}
	if status != contract.StatusActive && status != contract.StatusInProgress && status != contract.StatusPendingPay {
		return false
	}
	return endDate == nil || !endDate.Before(today)
}

// ContractDashboard 汇总普通来源合同的经营看板数据。
// 参数：ctx为上下文，tenantID为租户，ownerUserID为责任人，today为业务日期，limit为明细上限。
// 返回值：看板统计与明细或数据库错误。
func (r *Repository) ContractDashboard(ctx context.Context, tenantID, ownerUserID string, today time.Time, limit int) (contract.Dashboard, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	contractScope := func() *gorm.DB {
		query := ordinaryContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), "").Where("tenant_id = ?", tenantID)
		if ownerUserID != "" {
			query = query.Where("owner_user_id = ?", ownerUserID)
		}
		return query
	}
	approvalScope := func() *gorm.DB {
		query := r.db.WithContext(ctx).Model(&approvalInstanceRecord{}).Where("tenant_id = ?", tenantID)
		if ownerUserID != "" {
			ownedContracts := r.db.WithContext(ctx).Model(&contractRecord{}).
				Select("id").Where("tenant_id = ? AND owner_user_id = ?", tenantID, ownerUserID)
			query = query.Where("contract_id IN (?)", ownedContracts)
		}
		return query
	}
	currencyTotals, totalContracts, totalAmount, err := dashboardCurrencyTotals(contractScope())
	if err != nil {
		return contract.Dashboard{}, err
	}

	var approvalContractIDs []string
	if err := approvalScope().Distinct("contract_id").Where("status = ?", approval.StatusRunning).
		Pluck("contract_id", &approvalContractIDs).Error; err != nil {
		return contract.Dashboard{}, err
	}

	activeStatuses := dashboardActiveStatuses()
	var activeCount, expiredCount int64
	if err := dashboardActiveScope(contractScope(), activeStatuses, today).Count(&activeCount).Error; err != nil {
		return contract.Dashboard{}, err
	}
	if err := contractScope().
		Where("status IN ? AND end_date IS NOT NULL AND end_date < ?", activeStatuses, today).
		Count(&expiredCount).Error; err != nil {
		return contract.Dashboard{}, err
	}

	var records []contractRecord
	if err := contractScope().Select("id", "contract_number", "title", "contract_type", "service_type", "customer_credit_level", "owner_display_name", "amount_minor", "currency", "content", "status", "start_date", "end_date", "created_at", "updated_at").
		Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return contract.Dashboard{}, err
	}
	inApproval := make(map[string]bool, len(approvalContractIDs))
	for _, id := range approvalContractIDs {
		inApproval[id] = true
	}
	items := make([]contract.DashboardContract, 0, len(records))
	for _, record := range records {
		statusIsActive := dashboardStatusIsActive(contract.Status(record.Status), record.EndDate, today)
		expired := contract.Status(record.Status) != contract.StatusArchived && statusIsActive && record.EndDate != nil && record.EndDate.Before(today)
		item := contract.DashboardContract{ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Title: record.Title, Type: record.ContractType, ServiceType: record.ServiceType, OwnerDisplayName: record.OwnerDisplayName, AmountMinor: record.AmountMinor, Currency: record.Currency, Content: record.Content, Status: contract.Status(record.Status), StartDate: record.StartDate, EndDate: record.EndDate, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, InApproval: inApproval[record.ID], ActiveUnexpired: statusIsActive && !expired, Expired: expired}
		if record.CustomerCreditLevel != nil {
			item.CustomerCreditLevel = *record.CustomerCreditLevel
		}
		items = append(items, item)
	}
	return contract.Dashboard{
		TotalAmountMinor: totalAmount, CurrencyTotals: currencyTotals, TotalContracts: totalContracts,
		ApprovalContracts: int64(len(approvalContractIDs)), ActiveContracts: activeCount, ExpiredContracts: expiredCount,
		Contracts: items, ContractDetailLimited: totalContracts > int64(limit),
	}, nil
}

// ContractDashboardScoped 按可信组织及个人范围汇总普通合同经营看板。
// 参数：ctx为上下文，filter为可信范围，today为业务日期，limit为明细上限。
// 返回值：范围内统计及明细，或范围与数据库错误。
func (r *Repository) ContractDashboardScoped(ctx context.Context, filter contract.ScopeFilter, today time.Time, limit int) (contract.Dashboard, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	contractScope := func() *gorm.DB {
		return ordinaryContractScope(applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter), "")
	}
	currencyTotals, totalContracts, totalAmount, err := dashboardCurrencyTotals(contractScope())
	if err != nil {
		return contract.Dashboard{}, err
	}
	ownedContracts := contractScope().Select("id")
	var approvalContractIDs []string
	if err := r.db.WithContext(ctx).Model(&approvalInstanceRecord{}).Distinct("contract_id").
		Where("tenant_id = ? AND contract_id IN (?) AND status = ?", filter.TenantID, ownedContracts, approval.StatusRunning).
		Pluck("contract_id", &approvalContractIDs).Error; err != nil {
		return contract.Dashboard{}, err
	}
	activeStatuses := dashboardActiveStatuses()
	var activeCount, expiredCount int64
	if err := dashboardActiveScope(contractScope(), activeStatuses, today).Count(&activeCount).Error; err != nil {
		return contract.Dashboard{}, err
	}
	if err := contractScope().Where("status IN ? AND end_date IS NOT NULL AND end_date < ?", activeStatuses, today).Count(&expiredCount).Error; err != nil {
		return contract.Dashboard{}, err
	}
	var records []contractRecord
	if err := contractScope().Select("id", "contract_number", "title", "contract_type", "service_type", "customer_credit_level", "owner_display_name", "amount_minor", "currency", "content", "status", "start_date", "end_date", "created_at", "updated_at").Order("updated_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return contract.Dashboard{}, err
	}
	inApproval := make(map[string]bool, len(approvalContractIDs))
	for _, id := range approvalContractIDs {
		inApproval[id] = true
	}
	items := make([]contract.DashboardContract, 0, len(records))
	for _, record := range records {
		statusIsActive := dashboardStatusIsActive(contract.Status(record.Status), record.EndDate, today)
		expired := contract.Status(record.Status) != contract.StatusArchived && statusIsActive && record.EndDate != nil && record.EndDate.Before(today)
		item := contract.DashboardContract{ID: record.ID, Number: valueOrEmpty(record.ContractNumber), Title: record.Title, Type: record.ContractType, ServiceType: record.ServiceType, OwnerDisplayName: record.OwnerDisplayName, AmountMinor: record.AmountMinor, Currency: record.Currency, Content: record.Content, Status: contract.Status(record.Status), StartDate: record.StartDate, EndDate: record.EndDate, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, InApproval: inApproval[record.ID], ActiveUnexpired: statusIsActive && !expired, Expired: expired}
		if record.CustomerCreditLevel != nil {
			item.CustomerCreditLevel = *record.CustomerCreditLevel
		}
		items = append(items, item)
	}
	return contract.Dashboard{TotalAmountMinor: totalAmount, CurrencyTotals: currencyTotals, TotalContracts: totalContracts, ApprovalContracts: int64(len(approvalContractIDs)), ActiveContracts: activeCount, ExpiredContracts: expiredCount, Contracts: items, ContractDetailLimited: totalContracts > int64(limit)}, nil
}

func dashboardCurrencyTotals(scope *gorm.DB) ([]contract.CurrencyTotal, int64, int64, error) {
	type row struct {
		Currency         string
		TotalContracts   int64
		TotalAmountMinor int64
	}
	var rows []row
	if err := scope.Select("currency, COUNT(*) AS total_contracts, COALESCE(SUM(amount_minor), 0) AS total_amount_minor").
		Group("currency").Order("currency ASC").Scan(&rows).Error; err != nil {
		return nil, 0, 0, err
	}
	totals := make([]contract.CurrencyTotal, 0, len(rows))
	var contracts, amount int64
	for _, item := range rows {
		currency := strings.ToUpper(strings.TrimSpace(item.Currency))
		if currency == "" {
			currency = "UNKNOWN"
		}
		totals = append(totals, contract.CurrencyTotal{Currency: currency, AmountMinor: item.TotalAmountMinor, Contracts: item.TotalContracts})
		contracts += item.TotalContracts
		amount += item.TotalAmountMinor
	}
	return totals, contracts, amount, nil
}

func (r *Repository) GetApprovalMeta(ctx context.Context, tenantID, id string) (approval.Meta, error) {
	var record approvalInstanceRecord
	err := r.db.WithContext(ctx).
		Select("id", "tenant_id", "contract_id", "kind", "status", "applicant_user_id", "applicant_display_name", "temporal_workflow_id", "temporal_run_id", "from_status", "target_status", "reason", "rule_id", "rule_version").
		Where("tenant_id = ? AND id = ?", tenantID, id).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return approval.Meta{}, apperrors.ErrNotFound
	}
	if err != nil {
		return approval.Meta{}, err
	}
	return approval.Meta{
		ID: record.ID, TenantID: record.TenantID, ContractID: record.ContractID,
		Kind: approval.Kind(record.Kind), Status: approval.Status(record.Status),
		ApplicantUserID: record.ApplicantUserID, ApplicantDisplayName: record.ApplicantDisplayName, WorkflowID: record.TemporalWorkflowID, RunID: record.TemporalRunID,
		FromStatus: record.FromStatus, TargetStatus: record.TargetStatus,
		Reason: valueOrEmpty(record.Reason), RuleID: valueOrEmpty(record.RuleID), RuleVersion: uintValueOrZero(record.RuleVersion),
	}, nil
}

// GetApprovalRuntimeState 读取审批实例最后一次持久化的运行状态；工作流执行故障时，
// 管理端可据此判断是否已到达终态并做幂等收口。
func (r *Repository) GetApprovalRuntimeState(ctx context.Context, tenantID, approvalID string) (workflows.ApprovalState, bool, error) {
	var record approvalInstanceRecord
	err := r.db.WithContext(ctx).Select("runtime_state_json").Where("tenant_id = ? AND id = ?", tenantID, approvalID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return workflows.ApprovalState{}, false, apperrors.ErrNotFound
	}
	if err != nil {
		return workflows.ApprovalState{}, false, err
	}
	if len(record.RuntimeStateJSON) == 0 {
		return workflows.ApprovalState{}, false, nil
	}
	var state workflows.ApprovalState
	if err := json.Unmarshal(record.RuntimeStateJSON, &state); err != nil {
		return workflows.ApprovalState{}, false, fmt.Errorf("decode approval runtime state %s: %w", approvalID, err)
	}
	return state, true, nil
}

// GetApprovalSnapshot 读取审批发起时的合同快照；历史上没有快照的审批返回 found=false，
// 由应用层降级到当前合同，避免把缺失快照误判成合同不存在。
func (r *Repository) GetApprovalSnapshot(ctx context.Context, tenantID, approvalID string) (contract.Contract, bool, error) {
	var record approvalSnapshotRecord
	err := r.db.WithContext(ctx).Where("tenant_id = ? AND approval_id = ?", tenantID, approvalID).Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return contract.Contract{}, false, nil
	}
	if err != nil {
		return contract.Contract{}, false, err
	}
	var snapshot contract.Contract
	if err := json.Unmarshal(record.ContractJSON, &snapshot); err != nil {
		return contract.Contract{}, false, fmt.Errorf("decode approval snapshot %s: %w", approvalID, err)
	}
	snapshot.Document = record.RenderedDocument
	snapshot.CanEditDraft = false
	return snapshot, true, nil
}

func (r *Repository) ListApprovalActions(ctx context.Context, tenantID, approvalID string) ([]approval.Action, error) {
	var records []approvalActionRecord
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND approval_id = ?", tenantID, approvalID).
		Order("occurred_at ASC, id ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]approval.Action, 0, len(records))
	for _, record := range records {
		result = append(result, approval.Action{
			ID: record.ID, CommandID: record.CommandID, NodeID: valueOrEmpty(record.NodeID), Action: record.Action,
			ActorUserID: record.ActorUserID, ActorDisplayName: record.ActorDisplayName, Comment: valueOrEmpty(record.Comment), OccurredAt: record.OccurredAt,
		})
	}
	return result, nil
}

func (r *Repository) ListApprovals(ctx context.Context, tenantID, applicantUserID string, limit int) ([]approval.Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var records []approvalInstanceRecord
	if err := r.db.WithContext(ctx).
		Select("id", "contract_id", "applicant_user_id", "applicant_display_name", "kind", "status", "current_node_index", "created_at", "updated_at").
		Where("tenant_id = ? AND applicant_user_id = ?", tenantID, applicantUserID).
		Order("created_at DESC").Limit(limit).Find(&records).Error; err != nil {
		return nil, err
	}
	result := make([]approval.Summary, 0, len(records))
	for _, record := range records {
		result = append(result, approval.Summary{
			ApprovalID: record.ID, ContractID: record.ContractID, ApplicantUserID: record.ApplicantUserID, ApplicantDisplayName: record.ApplicantDisplayName,
			Kind: approval.Kind(record.Kind), Status: approval.Status(record.Status),
			CurrentNodeIndex: record.CurrentNodeIndex, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		})
	}
	return result, nil
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func uintValueOrZero(value *uint64) uint64 {
	if value == nil {
		return 0
	}
	return *value
}

// assignedApprovalTaskStatuses 使受理人可看到即将到来的任务。
// 只有有效任务可操作；待处理任务仅供只读预览。
func assignedApprovalTaskStatuses() []approval.NodeStatus {
	return []approval.NodeStatus{approval.NodeActive, approval.NodePending}
}

func (r *Repository) ListTasks(ctx context.Context, tenantID, userID string, limit int) ([]approval.Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	type taskRow struct {
		ApprovalID, ContractID, NodeID, NodeName, AssigneeUserID string
		Kind, Status                                             string
		NodeIndex                                                int
		CreatedAt                                                time.Time
	}
	var rows []taskRow
	err := r.db.WithContext(ctx).Table("con_approval_task AS t").
		Select("t.approval_id, i.contract_id, t.node_id, t.node_name, t.assignee_user_id, i.kind, t.status, t.node_index, i.created_at").
		Joins("JOIN con_approval_instance AS i ON i.id = t.approval_id").
		Where("i.tenant_id = ? AND t.assignee_user_id = ? AND t.status IN ?", tenantID, userID, assignedApprovalTaskStatuses()).
		Order("CASE WHEN t.status = 'active' THEN 0 ELSE 1 END").
		Order("i.created_at, t.node_index").Limit(limit).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make([]approval.Task, 0, len(rows))
	for _, row := range rows {
		result = append(result, approval.Task{ApprovalID: row.ApprovalID, ContractID: row.ContractID, NodeID: row.NodeID, NodeName: row.NodeName, AssigneeUserID: row.AssigneeUserID, Kind: approval.Kind(row.Kind), Status: approval.NodeStatus(row.Status), NodeIndex: row.NodeIndex, CreatedAt: row.CreatedAt})
	}
	return result, nil
}

// StartApproval 在源事务中启动普通合同审批并冻结对应原件快照。
// 参数：ctx为上下文，in为工作流已冻结的审批输入。
// 返回值：幂等启动结果或来源、版本、状态及数据库错误。
func (r *Repository) StartApproval(ctx context.Context, in workflows.StartApprovalActivityInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing approvalInstanceRecord
		err := tx.Select("id").Where("id = ?", in.ApprovalID).Take(&existing).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var current contractRecord
		// 读取完整合同行（含正文/文档），用于写入审批提交快照。
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", in.TenantID, in.ContractID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !ordinaryContractRecord(current) {
			return apperrors.ErrStateConflict
		}
		if current.Version != in.ExpectedVersion {
			return apperrors.ErrVersionConflict
		}
		if contract.Status(current.Status) != in.FromStatus {
			return fmt.Errorf("%w: expected %s, got %s", apperrors.ErrStateConflict, in.FromStatus, current.Status)
		}
		if in.Kind == approval.KindContract {
			if in.ContentHash == "" || current.ContentHash == nil || *current.ContentHash != in.ContentHash {
				return fmt.Errorf("%w: contract SHA-256 mismatch", apperrors.ErrStateConflict)
			}
			if err := updateStatus(tx, in.TenantID, in.ContractID, in.FromStatus, in.TargetStatus, in.ApplicantUserID); err != nil {
				return err
			}
			if err := insertLifecycle(tx, in, in.FromStatus, in.TargetStatus, in.ApplicantUserID, "submitted for approval", in.ApprovalID+":submitted"); err != nil {
				return err
			}
		} else if in.Kind == approval.KindStatusChange {
			var active int64
			if err := tx.Model(&approvalInstanceRecord{}).
				Where("tenant_id = ? AND contract_id = ? AND kind = ? AND status = ?", in.TenantID, in.ContractID, approval.KindStatusChange, approval.StatusRunning).
				Count(&active).Error; err != nil {
				return err
			}
			if active > 0 {
				return apperrors.ErrStateConflict
			}
		}
		nodesJSON, err := json.Marshal(in.Nodes)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		instance := approvalInstanceRecord{
			ID: in.ApprovalID, TenantID: in.TenantID, ContractID: in.ContractID,
			Kind: string(in.Kind), Status: string(approval.StatusRunning), ApplicantUserID: in.ApplicantUserID, ApplicantDisplayName: in.ApplicantDisplayName,
			FromStatus: string(in.FromStatus), TargetStatus: string(in.TargetStatus), Reason: stringPtr(in.Reason),
			RuleID: stringPtr(in.RuleID), RuleVersion: uintPtr(in.RuleVersion), ContentHash: stringPtr(in.ContentHash),
			NodesJSON: nodesJSON, CurrentNodeIndex: 0, TemporalWorkflowID: in.WorkflowID, TemporalRunID: in.RunID,
			CreatedAt: now, UpdatedAt: now,
		}
		if in.Kind == approval.KindStatusChange {
			instance.ActiveStatusChangeKey = stringPtr(in.TenantID + ":" + in.ContractID)
		}
		if err := tx.Create(&instance).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return apperrors.ErrStateConflict
			}
			return err
		}
		// 审批历史只认发起时的合同内容：快照与审批实例、状态流转同事务写入。
		snapshotContract := contractFromRecord(current)
		var termRows []paymentTermRecord
		if err := tx.Where("tenant_id = ? AND contract_id = ?", in.TenantID, in.ContractID).
			Order("installment_no ASC").Find(&termRows).Error; err != nil {
			return err
		}
		snapshotContract.PaymentTerms = make([]contract.PaymentTerm, 0, len(termRows))
		for _, term := range termRows {
			snapshotContract.PaymentTerms = append(snapshotContract.PaymentTerms, contract.PaymentTerm{
				InstallmentNo: int(term.InstallmentNo), DueDate: term.DueDate.Format("2006-01-02"), Amount: formatAmountMinor(term.AmountMinor),
			})
		}
		snapshotContract.Document = nil
		snapshotContract.CanEditDraft = false
		snapshotJSON, err := json.Marshal(snapshotContract)
		if err != nil {
			return err
		}
		if err := tx.Create(&approvalSnapshotRecord{
			ApprovalID: in.ApprovalID, TenantID: in.TenantID, ContractID: in.ContractID,
			ContractVersion: current.Version, ContentHash: current.ContentHash, ContractJSON: snapshotJSON,
			RenderedDocument: current.RenderedDocument, RuleID: stringPtr(in.RuleID), RuleVersion: uintPtr(in.RuleVersion),
			CreatedAt: now,
		}).Error; err != nil {
			return err
		}
		tasks := initialTasks(in.ApprovalID, in.Nodes, now)
		if len(tasks) > 0 {
			return tx.Create(&tasks).Error
		}
		return nil
	})
}

func (r *Repository) RecordCommand(ctx context.Context, in workflows.RecordCommandActivityInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing approvalActionRecord
		err := tx.Select("id").Where("command_id = ?", in.Command.CommandID).Take(&existing).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		stateJSON, err := json.Marshal(in.State)
		if err != nil {
			return err
		}
		payloadJSON, err := json.Marshal(in.Command)
		if err != nil {
			return err
		}
		when := in.Command.OccurredAt.UTC()
		if when.IsZero() {
			when = time.Now().UTC()
		}
		action := approvalActionRecord{
			ID: newID(), TenantID: in.TenantID, ApprovalID: in.ApprovalID, ContractID: in.ContractID,
			NodeID: stringPtr(in.NodeID), CommandID: in.Command.CommandID, Action: string(in.Command.Action),
			ActorUserID: in.Command.ActorUserID, ActorDisplayName: in.Command.ActorDisplayName, Comment: stringPtr(in.Command.Comment), PayloadJSON: payloadJSON, OccurredAt: when,
		}
		if err := tx.Create(&action).Error; err != nil {
			return err
		}
		result := tx.Model(&approvalInstanceRecord{}).Where("tenant_id = ? AND id = ?", in.TenantID, in.ApprovalID).
			Updates(map[string]any{"runtime_state_json": stateJSON, "current_node_index": in.State.CurrentNodeIndex, "status": in.State.Status, "updated_at": time.Now().UTC()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrNotFound
		}
		if err := syncTasks(tx, in.ApprovalID, in.State.Nodes); err != nil {
			return err
		}
		// 审批命令（含签署、转派、退回、催办）也是关键业务变更：
		// 与命令记录同事务写审计 outbox，动作按命令区分，便于按签署/转派检索。
		auditPayload, err := json.Marshal(map[string]any{
			"approval_id": in.ApprovalID, "command_id": in.Command.CommandID,
			"command": strings.ToUpper(string(in.Command.Action)), "node_id": in.NodeID,
			"countersign": string(in.Command.Countersign),
		})
		if err != nil {
			return err
		}
		meta := auditctx.FromContext(ctx)
		return insertAuditOutbox(tx, in.TenantID, newID(), AuditOutboxEntry{
			ActorID: in.Command.ActorUserID, Action: "CONTRACT_MANAGEMENT:APPROVAL_" + strings.ToUpper(string(in.Command.Action)),
			ResourceType: "contract", ResourceID: in.ContractID, Payload: auditPayload,
			LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
		})
	})
}

// CompleteApproval 应用普通审批终决议并按文档格式处理编号及业务派生。
// 参数：ctx为上下文，in为可信工作流审批结果。
// 返回值：幂等应用结果或来源、版本、原件和数据库错误。
func (r *Repository) CompleteApproval(ctx context.Context, in workflows.CompleteApprovalActivityInput) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var instance approvalInstanceRecord
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "kind", "from_status", "target_status", "content_hash", "completion_applied").
			Where("tenant_id = ? AND id = ?", in.TenantID, in.ApprovalID).Take(&instance).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if instance.CompletionApplied {
			return nil
		}

		var current contractRecord
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "status", "content_hash", "contract_number", "contract_number_format", "rendered_document", "document_format", "content", "template_values_json", "origin_kind", "import_mode", "import_policy_json").
			Where("tenant_id = ? AND id = ?", in.TenantID, in.ContractID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !ordinaryContractRecord(current) {
			return apperrors.ErrStateConflict
		}
		actor := in.ActorUserID
		if actor == "" {
			actor = "SYSTEM"
		}
		activityInput := workflows.StartApprovalActivityInput{ApprovalID: in.ApprovalID, TenantID: in.TenantID, ContractID: in.ContractID, WorkflowID: in.WorkflowID}
		kind, from, target := approval.Kind(instance.Kind), contract.Status(instance.FromStatus), contract.Status(instance.TargetStatus)
		if kind == approval.KindContract {
			if contract.Status(current.Status) != contract.StatusPending {
				return fmt.Errorf("%w: expected pending, got %s", apperrors.ErrStateConflict, current.Status)
			}
			if instance.ContentHash == nil || current.ContentHash == nil || *instance.ContentHash == "" || *current.ContentHash != *instance.ContentHash {
				return fmt.Errorf("%w: contract changed during approval", apperrors.ErrStateConflict)
			}
			if in.Status == approval.StatusApproved {
				if current.ContractNumber == nil {
					number := formatContractNumber(current.ContractNumberFormat, current.ID, time.Now().UTC())
					updates, err := approvedNumberUpdates(current, number)
					if err != nil {
						return err
					}
					if err := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ? AND contract_number IS NULL", in.TenantID, in.ContractID).Updates(updates).Error; err != nil {
						return err
					}
				}
				if err := updateStatus(tx, in.TenantID, in.ContractID, contract.StatusPending, contract.StatusApproved, actor); err != nil {
					return err
				}
				if err := insertLifecycle(tx, activityInput, contract.StatusPending, contract.StatusApproved, actor, in.Reason, in.ApprovalID+":approved"); err != nil {
					return err
				}
				if err := updateStatus(tx, in.TenantID, in.ContractID, contract.StatusApproved, contract.StatusActive, actor); err != nil {
					return err
				}
				if err := insertLifecycle(tx, activityInput, contract.StatusApproved, contract.StatusActive, actor, "approval completed; contract activated", in.ApprovalID+":activated"); err != nil {
					return err
				}
				if err := enqueueProjectActivation(tx, in.TenantID, in.ContractID); err != nil {
					return err
				}
				// 财务生效事件（contract.financial_effective.v1）与项目投递同源同事务。
				// 未维护付款计划或合计不符时 enqueueFinancialEffective 保持静默不写行，
				// 由 ReconcileFinancialEffectives 在条件具备后补建，绝不合成财务分期。
				if err := enqueueFinancialEffective(tx, in.TenantID, in.ContractID); err != nil {
					return err
				}
			} else {
				if err := updateStatus(tx, in.TenantID, in.ContractID, contract.StatusPending, contract.StatusDraft, actor); err != nil {
					return err
				}
				if err := insertLifecycle(tx, activityInput, contract.StatusPending, contract.StatusDraft, actor, in.Reason, in.ApprovalID+":"+string(in.Status)); err != nil {
					return err
				}
			}
		} else if in.Status == approval.StatusApproved {
			if contract.Status(current.Status) != from || in.TargetStatus != target {
				return fmt.Errorf("%w: stale status change", apperrors.ErrStateConflict)
			}
			if err := updateStatus(tx, in.TenantID, in.ContractID, from, target, actor); err != nil {
				return err
			}
			if err := insertLifecycle(tx, activityInput, from, target, actor, in.Reason, in.ApprovalID+":status-applied"); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		result := tx.Model(&approvalInstanceRecord{}).Where("id = ?", in.ApprovalID).
			Updates(map[string]any{"status": in.Status, "active_status_change_key": nil, "completion_applied": true, "completed_at": now, "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return apperrors.ErrNotFound
		}
		return tx.Model(&approvalTaskRecord{}).Where("approval_id = ? AND status = ?", in.ApprovalID, approval.NodeActive).
			Updates(map[string]any{"status": approval.NodeSkipped, "completed_at": now}).Error
	})
}

func (r *Repository) CreateNotification(ctx context.Context, in workflows.NotifyActivityInput) error {
	now := time.Now().UTC()
	records := make([]notificationOutboxRecord, 0, len(in.Recipients)+len(in.RoleRecipients))
	for _, recipient := range uniqueStrings(in.Recipients) {
		records = append(records, notificationOutboxRecord{
			ID: newID(), TenantID: in.TenantID, RecipientKey: "user:" + recipient, RecipientUserID: stringPtr(recipient),
			NotificationType: in.Type, Title: in.Title, Content: in.Content, ContractID: stringPtr(in.ContractID),
			ApprovalID: stringPtr(in.ApprovalID), DedupeKey: in.DedupeKey, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now,
		})
	}
	for _, roleCode := range uniqueStrings(in.RoleRecipients) {
		records = append(records, notificationOutboxRecord{
			ID: newID(), TenantID: in.TenantID, RecipientKey: "role:" + roleCode, RecipientRoleCode: stringPtr(roleCode),
			NotificationType: in.Type, Title: in.Title, Content: in.Content, ContractID: stringPtr(in.ContractID),
			ApprovalID: stringPtr(in.ApprovalID), DedupeKey: in.DedupeKey, DeliveryStatus: "pending", NextAttemptAt: now, CreatedAt: now,
		})
	}
	if len(records) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&records).Error
}

// updateStatus 更新状态并在同一事务同步责任撤销或恢复。
// 参数：tx 为合同行事务，租户和合同限定对象，from/to 为状态，actor 为操作者。
// 返回值：状态、并发或责任事件写入错误。
func updateStatus(tx *gorm.DB, tenantID, contractID string, from, to contract.Status, actor string) error {
	if err := contract.ValidateTransition(from, to); err != nil {
		return err
	}
	result := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, contractID, from).
		Updates(map[string]any{"status": to, "version": gorm.Expr("version + ?", 1), "updated_at": time.Now().UTC(), "updated_by": actor})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return apperrors.ErrStateConflict
	}
	return enqueueContractResponsibility(tx, tenantID, contractID)
}

func insertLifecycle(tx *gorm.DB, in workflows.StartApprovalActivityInput, from, to contract.Status, actor, reason, key string) error {
	record := lifecycleEventRecord{
		ID: newID(), TenantID: in.TenantID, ContractID: in.ContractID, FromStatus: string(from), ToStatus: string(to),
		ActorUserID: stringPtr(actor), Reason: stringPtr(reason), ApprovalID: stringPtr(in.ApprovalID),
		WorkflowID: stringPtr(in.WorkflowID), IdempotencyKey: key, OccurredAt: time.Now().UTC(),
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error; err != nil {
		return err
	}
	// 批次 2：审计事件与业务状态变更同事务落库，平台不可用时由 dispatcher 重试，
	// 不再出现"业务已提交、审计同步失败返 503"的不一致。
	payload, _ := json.Marshal(map[string]any{
		"contract_id": in.ContractID, "from_status": string(from), "to_status": string(to), "reason": reason,
	})
	meta := auditctx.FromContext(tx.Statement.Context)
	entry := AuditOutboxEntry{
		ActorID: actor, Action: "CONTRACT_MANAGEMENT:STATUS_CHANGE",
		ResourceType: "contract", ResourceID: in.ContractID, Payload: payload,
		LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
	}
	return insertAuditOutbox(tx, in.TenantID, newID(), entry)
}

func syncTasks(tx *gorm.DB, approvalID string, nodes []workflows.RuntimeNode) error {
	if err := tx.Where("approval_id = ?", approvalID).Delete(&approvalTaskRecord{}).Error; err != nil {
		return err
	}
	records := make([]approvalTaskRecord, 0)
	for index, runtime := range nodes {
		for _, userID := range uniqueStrings(runtime.Node.AssigneeIDs) {
			records = append(records, approvalTaskRecord{ApprovalID: approvalID, NodeID: runtime.Node.ID, AssigneeUserID: userID, NodeName: runtime.Node.Name, NodeIndex: index, Status: string(runtime.Status), Approved: runtime.ApprovedBy[userID], StartedAt: timePtr(runtime.StartedAt), CompletedAt: timePtr(runtime.CompletedAt)})
		}
	}
	if len(records) == 0 {
		return nil
	}
	return tx.Create(&records).Error
}

func initialTasks(approvalID string, nodes []approval.Node, now time.Time) []approvalTaskRecord {
	records := make([]approvalTaskRecord, 0)
	for index, node := range nodes {
		status := approval.NodePending
		var startedAt *time.Time
		if index == 0 {
			status, startedAt = approval.NodeActive, &now
		}
		for _, userID := range uniqueStrings(node.AssigneeIDs) {
			records = append(records, approvalTaskRecord{ApprovalID: approvalID, NodeID: node.ID, AssigneeUserID: userID, NodeName: node.Name, NodeIndex: index, Status: string(status), StartedAt: startedAt})
		}
	}
	return records
}

// contractFromRecord 将完整持久记录恢复为含来源和文档分型的合同。
// 参数：record为同租户查询得到的权威合同行。
// 返回值：业务合同；损坏历史策略保留为不可放行版本。
func contractFromRecord(record contractRecord) contract.Contract {
	result := contract.Contract{ID: record.ID, TenantID: record.TenantID, Number: valueOrEmpty(record.ContractNumber), NumberFormat: record.ContractNumberFormat, Title: record.Title, Type: record.ContractType, ServiceType: record.ServiceType, OpportunityID: valueOrEmpty(record.OpportunityID), OpportunityName: valueOrEmpty(record.OpportunityName), CRMCustomerID: uintValueOrZero(record.CRMCustomerID), CustomerName: valueOrEmpty(record.CustomerName), CustomerAddress: valueOrEmpty(record.CustomerAddress), CustomerContact: valueOrEmpty(record.CustomerContact), CustomerPhone: valueOrEmpty(record.CustomerPhone), OwnerIdentityID: valueOrEmpty(record.OwnerIdentityID), OwnerOrgID: valueOrEmpty(record.OwnerOrgID), ProjectID: valueOrEmpty(record.ProjectID), OwnerUserID: record.OwnerUserID, OwnerDisplayName: record.OwnerDisplayName, AmountMinor: record.AmountMinor, Currency: record.Currency, Content: record.Content, Document: record.RenderedDocument, SourceFileID: record.SourceFileID, SourceFileStatus: record.SourceFileStatus, Status: contract.Status(record.Status), Version: record.Version, StartDate: record.StartDate, EndDate: record.EndDate, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
	result.CreatedBy = record.CreatedBy
	readContractSource(record, &result)
	if record.CustomerCreditLevel != nil {
		result.CustomerCreditLevel = *record.CustomerCreditLevel
	}
	if record.ContentHash != nil {
		result.ContentHash = *record.ContentHash
	}
	if record.TemplateID != nil {
		result.TemplateID = *record.TemplateID
	}
	if len(record.TemplateValuesJSON) > 0 {
		_ = json.Unmarshal(record.TemplateValuesJSON, &result.TemplateValues)
	}
	if len(record.SystemsJSON) > 0 {
		_ = json.Unmarshal(record.SystemsJSON, &result.Systems)
	}
	if len(record.ServiceItemsJSON) > 0 {
		_ = json.Unmarshal(record.ServiceItemsJSON, &result.ServiceItems)
	}
	if len(result.ServiceItems) == 0 && result.ServiceType != "" {
		result.ServiceItems = []contract.ServiceItem{{ServiceType: result.ServiceType, Systems: result.Systems}}
	}
	return result
}

func formatContractNumber(format, id string, now time.Time) string {
	if format == "" {
		format = contracttemplate.DefaultNumberFormat
	}
	id8 := id
	if len(id8) > 8 {
		id8 = id8[len(id8)-8:]
	}
	return strings.NewReplacer(
		"{YYYYMMDD}", now.Format("20060102"),
		"{YYYY}", now.Format("2006"),
		"{MM}", now.Format("01"),
		"{DD}", now.Format("02"),
		"{ID8}", id8,
	).Replace(format)
}

func newID() string { return ulid.Make().String() }
func uniqueStrings(values []string) []string {
	seen, result := map[string]bool{}, []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
