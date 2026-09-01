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
	return contractFromRecord(record), nil
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
	return contractFromRecord(record), nil
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
	return result, nil
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
	return result, nil
}

func dashboardActiveStatuses() []contract.Status {
	return []contract.Status{contract.StatusActive, contract.StatusInProgress, contract.StatusPendingPay}
}

// Archived contracts remain valid business contracts for reporting. They are
// terminal and may have a past end date, so they must be counted separately
// from currently active statuses and must never be classified as expired.
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

func (r *Repository) ContractDashboard(ctx context.Context, tenantID, ownerUserID string, today time.Time, limit int) (contract.Dashboard, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var aggregate struct {
		TotalContracts   int64
		TotalAmountMinor int64
	}
	contractScope := func() *gorm.DB {
		query := r.db.WithContext(ctx).Model(&contractRecord{}).Where("tenant_id = ?", tenantID)
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
	if err := contractScope().
		Select("COUNT(*) AS total_contracts, COALESCE(SUM(amount_minor), 0) AS total_amount_minor").
		Scan(&aggregate).Error; err != nil {
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
		TotalAmountMinor: aggregate.TotalAmountMinor, TotalContracts: aggregate.TotalContracts,
		ApprovalContracts: int64(len(approvalContractIDs)), ActiveContracts: activeCount, ExpiredContracts: expiredCount,
		Contracts: items, ContractDetailLimited: aggregate.TotalContracts > int64(limit),
	}, nil
}

func (r *Repository) ContractDashboardScoped(ctx context.Context, filter contract.ScopeFilter, today time.Time, limit int) (contract.Dashboard, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	contractScope := func() *gorm.DB { return applyContractScope(r.db.WithContext(ctx).Model(&contractRecord{}), filter) }
	var aggregate struct {
		TotalContracts   int64
		TotalAmountMinor int64
	}
	if err := contractScope().Select("COUNT(*) AS total_contracts, COALESCE(SUM(amount_minor), 0) AS total_amount_minor").Scan(&aggregate).Error; err != nil {
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
	return contract.Dashboard{TotalAmountMinor: aggregate.TotalAmountMinor, TotalContracts: aggregate.TotalContracts, ApprovalContracts: int64(len(approvalContractIDs)), ActiveContracts: activeCount, ExpiredContracts: expiredCount, Contracts: items, ContractDetailLimited: aggregate.TotalContracts > int64(limit)}, nil
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

// assignedApprovalTaskStatuses keeps upcoming tasks visible to their assignee.
// Only active tasks are actionable; pending tasks are read-only previews.
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
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "tenant_id", "status", "version", "content_hash").
			Where("tenant_id = ? AND id = ?", in.TenantID, in.ContractID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
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
		return syncTasks(tx, in.ApprovalID, in.State.Nodes)
	})
}

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
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "status", "content_hash", "contract_number", "contract_number_format").
			Where("tenant_id = ? AND id = ?", in.TenantID, in.ContractID).Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return apperrors.ErrNotFound
		}
		if err != nil {
			return err
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
					if err := tx.Model(&contractRecord{}).Where("tenant_id = ? AND id = ? AND contract_number IS NULL", in.TenantID, in.ContractID).Update("contract_number", number).Error; err != nil {
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
	return nil
}

func insertLifecycle(tx *gorm.DB, in workflows.StartApprovalActivityInput, from, to contract.Status, actor, reason, key string) error {
	record := lifecycleEventRecord{
		ID: newID(), TenantID: in.TenantID, ContractID: in.ContractID, FromStatus: string(from), ToStatus: string(to),
		ActorUserID: stringPtr(actor), Reason: stringPtr(reason), ApprovalID: stringPtr(in.ApprovalID),
		WorkflowID: stringPtr(in.WorkflowID), IdempotencyKey: key, OccurredAt: time.Now().UTC(),
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&record).Error
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

func contractFromRecord(record contractRecord) contract.Contract {
	result := contract.Contract{ID: record.ID, TenantID: record.TenantID, Number: valueOrEmpty(record.ContractNumber), NumberFormat: record.ContractNumberFormat, Title: record.Title, Type: record.ContractType, ServiceType: record.ServiceType, OpportunityID: valueOrEmpty(record.OpportunityID), OpportunityName: valueOrEmpty(record.OpportunityName), CRMCustomerID: uintValueOrZero(record.CRMCustomerID), CustomerName: valueOrEmpty(record.CustomerName), CustomerAddress: valueOrEmpty(record.CustomerAddress), CustomerContact: valueOrEmpty(record.CustomerContact), CustomerPhone: valueOrEmpty(record.CustomerPhone), OwnerIdentityID: valueOrEmpty(record.OwnerIdentityID), OwnerOrgID: valueOrEmpty(record.OwnerOrgID), ProjectID: valueOrEmpty(record.ProjectID), OwnerUserID: record.OwnerUserID, OwnerDisplayName: record.OwnerDisplayName, AmountMinor: record.AmountMinor, Currency: record.Currency, Content: record.Content, Document: record.RenderedDocument, Status: contract.Status(record.Status), Version: record.Version, StartDate: record.StartDate, EndDate: record.EndDate, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
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
