package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

type Principal struct {
	TenantID, UserID string
	Roles            []string
	Permissions      map[string]bool
	RoleConfigHash   string
	AuthzRevision    uint64
}

func (p Principal) Has(permission string) bool { return p.Permissions[permission] }

type Repository interface {
	GetContract(context.Context, string, string) (contract.Contract, error)
	ListContracts(context.Context, string, string, string, int) ([]contract.Contract, error)
	CreateContract(context.Context, contract.Contract, string) error
	TransitionDirect(context.Context, string, string, uint64, contract.Status, string, string, string) error
	ListEnabledRules(context.Context, string) ([]approval.Rule, error)
	ListRules(context.Context, string) ([]approval.Rule, error)
	CreateRule(context.Context, approval.Rule, string) error
	UpdateRule(context.Context, approval.Rule, string) error
	DeleteRule(context.Context, string, string, uint64) error
	GetApprovalMeta(context.Context, string, string) (approval.Meta, error)
	ListApprovalActions(context.Context, string, string) ([]approval.Action, error)
	ListApprovals(context.Context, string, string, int) ([]approval.Summary, error)
	ListTasks(context.Context, string, string, int) ([]approval.Task, error)
}

type ApproverResolver interface {
	Resolve(roleCode string) []string
}

type Service struct {
	Repo             Repository
	Temporal         client.Client
	Approvers        ApproverResolver
	TaskQueue        string
	NodeTimeout      time.Duration
	ReminderInterval time.Duration
}

type StartResult struct {
	ApprovalID string `json:"approval_id"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

type ApprovalDetail struct {
	Meta     approval.Meta           `json:"meta"`
	State    workflows.ApprovalState `json:"state"`
	Contract contract.Contract       `json:"contract"`
	Actions  []approval.Action       `json:"actions"`
}

func (s *Service) CreateContract(ctx context.Context, actor Principal, c contract.Contract) (contract.Contract, error) {
	if !actor.Has("contract.create") {
		return c, ErrForbidden
	}
	if c.Number == "" || c.Title == "" || c.Type == "" || c.ServiceType == "" || c.AmountMinor < 0 || c.Content == "" {
		return c, ErrValidation
	}
	if c.Currency == "" {
		c.Currency = "CNY"
	}
	c.ID, c.TenantID, c.OwnerUserID, c.Status = ulid.Make().String(), actor.TenantID, actor.UserID, contract.StatusDraft
	hash := sha256.Sum256([]byte(c.Content))
	c.ContentHash = hex.EncodeToString(hash[:])
	if err := s.Repo.CreateContract(ctx, c, actor.UserID); err != nil {
		return c, err
	}
	c.Version = 1
	return c, nil
}

func (s *Service) SubmitContract(ctx context.Context, actor Principal, contractID string, termsIdentical bool) (StartResult, error) {
	if !actor.Has("contract.create") {
		return StartResult{}, ErrForbidden
	}
	c, err := s.Repo.GetContract(ctx, actor.TenantID, contractID)
	if err != nil {
		return StartResult{}, err
	}
	if c.OwnerUserID != actor.UserID {
		return StartResult{}, ErrForbidden
	}
	if c.Status != contract.StatusDraft {
		return StartResult{}, apperrors.ErrStateConflict
	}
	rules, err := s.Repo.ListEnabledRules(ctx, actor.TenantID)
	if err != nil {
		return StartResult{}, err
	}
	matched, err := approval.MatchHighest(rules, approval.Facts{AmountMinor: c.AmountMinor, ServiceType: c.ServiceType, CustomerCreditLevel: c.CustomerCreditLevel, ContractType: c.Type, TermsIdentical: termsIdentical})
	if err != nil {
		return StartResult{}, err
	}
	nodes := defaultNodes()
	var ruleID string
	var ruleVersion uint64
	if matched != nil {
		nodes, ruleID, ruleVersion = matched.Nodes, matched.ID, matched.Version
	}
	if err := s.resolveNodes(nodes); err != nil {
		return StartResult{}, err
	}
	approvalID := ulid.Make().String()
	workflowID := fmt.Sprintf("contract-approval:%s:%s:v%d", actor.TenantID, contractID, c.Version)
	in := workflows.ContractApprovalInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: contractID, ContractVersion: c.Version, ApplicantUserID: actor.UserID, ContentHash: c.ContentHash, RuleID: ruleID, RuleVersion: ruleVersion, Nodes: nodes, DefaultNodeTimeout: s.NodeTimeout, ReminderInterval: s.ReminderInterval}
	run, err := s.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: s.taskQueue(), WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, workflows.ContractApprovalWorkflowName, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return StartResult{}, apperrors.ErrStateConflict
		}
		return StartResult{}, err
	}
	return StartResult{ApprovalID: approvalID, WorkflowID: run.GetID(), RunID: run.GetRunID()}, nil
}

func (s *Service) ChangeStatus(ctx context.Context, actor Principal, contractID string, version uint64, target contract.Status, reason string) (StartResult, error) {
	if !actor.Has("contract.edit") {
		return StartResult{}, ErrForbidden
	}
	if reason == "" {
		return StartResult{}, ErrValidation
	}
	c, err := s.Repo.GetContract(ctx, actor.TenantID, contractID)
	if err != nil {
		return StartResult{}, err
	}
	if c.OwnerUserID != actor.UserID {
		return StartResult{}, ErrForbidden
	}
	if c.Version != version {
		return StartResult{}, apperrors.ErrVersionConflict
	}
	if err := contract.ValidateTransition(c.Status, target); err != nil {
		return StartResult{}, err
	}
	if !target.RequiresApproval() {
		key := ulid.Make().String()
		return StartResult{}, s.Repo.TransitionDirect(ctx, actor.TenantID, contractID, version, target, actor.UserID, reason, key)
	}
	admins := s.Approvers.Resolve("admin")
	if len(admins) == 0 {
		return StartResult{}, fmt.Errorf("no approver configured for admin")
	}
	approvalID := ulid.Make().String()
	workflowID := fmt.Sprintf("status-change:%s:%s:v%d", actor.TenantID, contractID, version)
	in := workflows.StatusChangeInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: contractID, ContractVersion: version, ApplicantUserID: actor.UserID, FromStatus: c.Status, TargetStatus: target, Reason: reason, AdminUserIDs: admins, Timeout: s.NodeTimeout}
	run, err := s.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: s.taskQueue(), WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, workflows.StatusChangeWorkflowName, in)
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStarted) {
			return StartResult{}, apperrors.ErrStateConflict
		}
		return StartResult{}, err
	}
	return StartResult{ApprovalID: approvalID, WorkflowID: run.GetID(), RunID: run.GetRunID()}, nil
}

func (s *Service) Command(ctx context.Context, actor Principal, approvalID string, command workflows.ApprovalCommand) error {
	meta, err := s.Repo.GetApprovalMeta(ctx, actor.TenantID, approvalID)
	if err != nil {
		return err
	}
	if meta.Status != approval.StatusRunning {
		return apperrors.ErrStateConflict
	}
	switch command.Action {
	case workflows.ActionWithdraw:
		if meta.ApplicantUserID != actor.UserID {
			return ErrForbidden
		}
	case workflows.ActionUrge:
		if meta.ApplicantUserID != actor.UserID && !actor.Has("approval.manage") {
			return ErrForbidden
		}
	case workflows.ActionComment:
		if !actor.Has("approval.view") && meta.ApplicantUserID != actor.UserID {
			return ErrForbidden
		}
	default:
		if !actor.Has("approval.process") {
			return ErrForbidden
		}
	}
	command.CommandID, command.ActorUserID, command.OccurredAt = ulid.Make().String(), actor.UserID, time.Now().UTC()
	return s.Temporal.SignalWorkflow(ctx, meta.WorkflowID, meta.RunID, workflows.CommandSignalName, command)
}

func (s *Service) GetApprovalState(ctx context.Context, actor Principal, approvalID string) (workflows.ApprovalState, error) {
	_, state, err := s.queryApprovalState(ctx, actor, approvalID)
	return state, err
}

func (s *Service) GetApprovalDetail(ctx context.Context, actor Principal, approvalID string) (ApprovalDetail, error) {
	meta, state, err := s.queryApprovalState(ctx, actor, approvalID)
	if err != nil {
		return ApprovalDetail{}, err
	}
	contractSnapshot, err := s.Repo.GetContract(ctx, actor.TenantID, meta.ContractID)
	if err != nil {
		return ApprovalDetail{}, err
	}
	actions, err := s.Repo.ListApprovalActions(ctx, actor.TenantID, approvalID)
	if err != nil {
		return ApprovalDetail{}, err
	}
	if actions == nil {
		actions = []approval.Action{}
	}
	return ApprovalDetail{Meta: meta, State: state, Contract: contractSnapshot, Actions: actions}, nil
}

func (s *Service) queryApprovalState(ctx context.Context, actor Principal, approvalID string) (approval.Meta, workflows.ApprovalState, error) {
	meta, err := s.Repo.GetApprovalMeta(ctx, actor.TenantID, approvalID)
	if err != nil {
		return approval.Meta{}, workflows.ApprovalState{}, err
	}
	if !actor.Has("approval.view") && !actor.Has("approval.process") && meta.ApplicantUserID != actor.UserID {
		return approval.Meta{}, workflows.ApprovalState{}, ErrForbidden
	}
	encoded, err := s.Temporal.QueryWorkflow(ctx, meta.WorkflowID, meta.RunID, workflows.StateQueryName)
	if err != nil {
		return approval.Meta{}, workflows.ApprovalState{}, err
	}
	var state workflows.ApprovalState
	if err := encoded.Get(&state); err != nil {
		return approval.Meta{}, workflows.ApprovalState{}, err
	}
	return meta, state, nil
}

func (s *Service) ListMyTasks(ctx context.Context, actor Principal, limit int) ([]approval.Task, error) {
	if !actor.Has("approval.process") {
		return nil, ErrForbidden
	}
	return s.Repo.ListTasks(ctx, actor.TenantID, actor.UserID, limit)
}

func (s *Service) ListMyApprovals(ctx context.Context, actor Principal, limit int) ([]approval.Summary, error) {
	if actor.TenantID == "" || actor.UserID == "" {
		return nil, ErrForbidden
	}
	return s.Repo.ListApprovals(ctx, actor.TenantID, actor.UserID, limit)
}

func (s *Service) GetContract(ctx context.Context, actor Principal, id string) (contract.Contract, error) {
	if !actor.Has("contract.read") {
		return contract.Contract{}, ErrForbidden
	}
	c, err := s.Repo.GetContract(ctx, actor.TenantID, id)
	if err != nil {
		return c, err
	}
	if c.OwnerUserID != actor.UserID {
		return contract.Contract{}, ErrForbidden
	}
	return c, nil
}

func (s *Service) ListContracts(ctx context.Context, actor Principal, _ string, status string, limit int) ([]contract.Contract, error) {
	if !actor.Has("contract.read") {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.Repo.ListContracts(ctx, actor.TenantID, actor.UserID, status, limit)
}

func (s *Service) ListRules(ctx context.Context, actor Principal) ([]approval.Rule, error) {
	if !actor.Has("approval.view") && !actor.Has("approval_rule.manage") {
		return nil, ErrForbidden
	}
	return s.Repo.ListRules(ctx, actor.TenantID)
}

func (s *Service) CreateRule(ctx context.Context, actor Principal, rule approval.Rule) (approval.Rule, error) {
	if !actor.Has("approval_rule.manage") {
		return rule, ErrForbidden
	}
	if err := validateRule(rule); err != nil {
		return rule, err
	}
	rule.ID, rule.TenantID, rule.Version = ulid.Make().String(), actor.TenantID, 1
	if err := s.Repo.CreateRule(ctx, rule, actor.UserID); err != nil {
		return rule, err
	}
	return rule, nil
}

func (s *Service) UpdateRule(ctx context.Context, actor Principal, rule approval.Rule) (approval.Rule, error) {
	if !actor.Has("approval_rule.manage") {
		return rule, ErrForbidden
	}
	if rule.ID == "" || rule.Version == 0 {
		return rule, ErrValidation
	}
	if err := validateRule(rule); err != nil {
		return rule, err
	}
	rule.TenantID = actor.TenantID
	if err := s.Repo.UpdateRule(ctx, rule, actor.UserID); err != nil {
		return rule, err
	}
	rule.Version++
	return rule, nil
}

func (s *Service) DeleteRule(ctx context.Context, actor Principal, id string, version uint64) error {
	if !actor.Has("approval_rule.manage") {
		return ErrForbidden
	}
	if id == "" || version == 0 {
		return ErrValidation
	}
	return s.Repo.DeleteRule(ctx, actor.TenantID, id, version)
}

func validateRule(rule approval.Rule) error {
	if rule.Name == "" || len(rule.Nodes) == 0 {
		return ErrValidation
	}
	if _, err := rule.Expression.Match(approval.Facts{}); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	seen := map[string]bool{}
	for _, node := range rule.Nodes {
		if node.ID == "" || node.Name == "" || node.RoleCode == "" || seen[node.ID] {
			return ErrValidation
		}
		if node.Countersign != "" && node.Countersign != approval.CountersignAll && node.Countersign != approval.CountersignAny {
			return ErrValidation
		}
		seen[node.ID] = true
	}
	return nil
}

func (s *Service) resolveNodes(nodes []approval.Node) error {
	for i := range nodes {
		if len(nodes[i].AssigneeIDs) == 0 {
			nodes[i].AssigneeIDs = s.Approvers.Resolve(nodes[i].RoleCode)
		}
		nodes[i].AssigneeIDs = unique(nodes[i].AssigneeIDs)
		if nodes[i].ID == "" {
			nodes[i].ID = fmt.Sprintf("node-%d", i+1)
		}
		if len(nodes[i].AssigneeIDs) == 0 {
			return fmt.Errorf("no approver configured for role %s", nodes[i].RoleCode)
		}
	}
	return nil
}

func defaultNodes() []approval.Node {
	return []approval.Node{
		{ID: "sales-director", Name: "销售总监审批", RoleCode: "sales_director", Countersign: approval.CountersignAll},
		{ID: "tech-director", Name: "技术总监审批", RoleCode: "tech_director", Countersign: approval.CountersignAll},
		{ID: "finance-director", Name: "财务总监审批", RoleCode: "finance_director", Countersign: approval.CountersignAll},
	}
}

func (s *Service) taskQueue() string {
	if s.TaskQueue == "" {
		return workflows.TaskQueue
	}
	return s.TaskQueue
}
func unique(values []string) []string {
	seen, result := map[string]bool{}, []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

var (
	ErrForbidden  = errors.New("forbidden")
	ErrValidation = errors.New("validation failed")
)
