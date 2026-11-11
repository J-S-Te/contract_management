package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"go.temporal.io/sdk/client"
)

type PricingRevisionRepository interface {
	AcceptPricingRevision(context.Context, workflows.StartApprovalActivityInput, contract.Contract, func(context.Context) (string, error)) error
}

// SubmitPricingRevision 受理已批准合同的显式定价修订并沿原审批节点独立审查。
// 参数：ctx/actor为请求及主体，id为合同，change为完整新金额计划和显式新基准。
// 返回值：已持久化审批身份；批准前当前合同金额和冻结基准保持原值。
func (s *Service) SubmitPricingRevision(ctx context.Context, actor Principal, id string, change ContractFinancialChange) (StartResult, error) {
	if !s.ContractFinancialChangeEnabled {
		return StartResult{}, ErrFinancialChangeDisabled
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return StartResult{}, err
	}
	filter, ok := actor.Scope("contract.edit")
	if !ok || actor.UserID == "" {
		return StartResult{}, ErrForbidden
	}
	current, err := s.getContractScoped(ctx, filter, id)
	if err != nil {
		return StartResult{}, err
	}
	if current.CounterpartySource == contract.CounterpartyLocal {
		return StartResult{}, ErrContractSourceConflict
	}
	if !contract.OperationalOrigin(current) || !FinancialChangeStatusEligible(current.Status) {
		return StartResult{}, apperrors.ErrStateConflict
	}
	if change.ExpectedVersion == 0 || change.ExpectedVersion != current.Version {
		return StartResult{}, apperrors.ErrVersionConflict
	}
	if change.Pricing == nil || !change.Pricing.Reselect || strings.TrimSpace(change.Pricing.BasisDate) == "" || strings.TrimSpace(change.Reason) == "" || len([]rune(change.Reason)) > 500 || change.AmountMinor <= 0 || change.Currency != "" && change.Currency != current.Currency || len(change.PaymentTerms) == 0 {
		return StartResult{}, ErrValidation
	}
	candidate := current
	candidate.ServiceItems = append([]contract.ServiceItem(nil), current.ServiceItems...)
	// 已生效服务即使未存SourceID，也已按项目投影规则公开身份；首次基准必须沿用。
	for i := range candidate.ServiceItems {
		if candidate.ServiceItems[i].SourceID == "" {
			candidate.ServiceItems[i].SourceID = contract.ProjectServiceBaseID(current.ID, i, current.ServiceItems[i])
		}
	}
	candidate.AmountMinor = change.AmountMinor
	candidate.Pricing = change.Pricing
	candidate.Pricing.Reason = change.Reason
	candidate.PaymentTerms, err = contract.NormalizePaymentTerms(change.PaymentTerms, change.AmountMinor, current.Currency)
	if err != nil {
		return StartResult{}, ErrValidation
	}
	candidate, err = s.preparePricing(ctx, actor, candidate, current.Pricing)
	if err != nil {
		return StartResult{}, err
	}
	directory, err := s.loadPersonnel(ctx, actor, []string{"admin"})
	if err != nil {
		return StartResult{}, err
	}
	admins := approversForRole(directory, "admin")
	if len(admins) == 0 {
		return StartResult{}, ErrForbidden
	}
	repo, ok := s.Repo.(PricingRevisionRepository)
	if !ok || s.Temporal == nil {
		return StartResult{}, ErrApprovalWorkflowUnavailable
	}
	approvalID := ulid.Make().String()
	workflowID := fmt.Sprintf("pricing-revision:%s:%s:v%d:%s", actor.TenantID, id, current.Version, approvalID)
	timeout := s.NodeTimeout
	if timeout <= 0 {
		timeout = 72 * time.Hour
	}
	in := workflows.StatusChangeInput{PricingRevision: true, RequireExisting: true, ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: id, ContractVersion: current.Version, ApplicantUserID: actor.UserID, ApplicantDisplayName: actor.DisplayName, FromStatus: current.Status, TargetStatus: current.Status, Reason: change.Reason, AdminUserIDs: admins, Timeout: timeout, Audit: auditMetaFromContext(ctx)}
	start := workflows.StartApprovalActivityInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: id, ExpectedVersion: current.Version, ApplicantUserID: actor.UserID, ApplicantDisplayName: actor.DisplayName, Kind: approval.KindStatusChange, FromStatus: current.Status, TargetStatus: current.Status, Reason: change.Reason, WorkflowID: workflowID, Audit: in.Audit, Nodes: []approval.Node{{ID: "admin", Name: "合同定价修订审批", RoleCode: "admin", AssigneeIDs: admins, Countersign: approval.CountersignAny, Timeout: timeout}}}
	var run client.WorkflowRun
	err = repo.AcceptPricingRevision(ctx, start, candidate, func(startCtx context.Context) (string, error) {
		bounded, cancel := context.WithTimeout(startCtx, 5*time.Second)
		defer cancel()
		var e error
		run, e = s.Temporal.ExecuteWorkflow(bounded, client.StartWorkflowOptions{ID: workflowID, TaskQueue: s.taskQueue()}, workflows.StatusChangeWorkflowName, in)
		if e != nil {
			return "", e
		}
		return run.GetRunID(), nil
	})
	if err != nil {
		return StartResult{}, err
	}
	return StartResult{ApprovalID: approvalID, WorkflowID: run.GetID(), RunID: run.GetRunID()}, nil
}
