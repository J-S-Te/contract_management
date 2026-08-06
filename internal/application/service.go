package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/docx"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	contracttemplate "github.com/j-s-te/contract-management/internal/domain/template"
	"github.com/j-s-te/contract-management/internal/workflows"
	"github.com/oklog/ulid/v2"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

type Principal struct {
	TenantID, UserID string
	DisplayName      string
	UserName         string
	Email            string
	UserDirectory    []UserReference
	Roles            []string
	Permissions      map[string]bool
	RoleConfigHash   string
	AuthzRevision    uint64
}

func (p Principal) Has(permission string) bool { return p.Permissions[permission] }

type Repository interface {
	GetContract(context.Context, string, string) (contract.Contract, error)
	ListContracts(context.Context, string, string, string, int) ([]contract.Contract, error)
	ListApprovedContracts(context.Context, string, int) ([]contract.Contract, error)
	SaveStampedDocument(context.Context, string, contract.StampedDocument) error
	GetStampedDocument(context.Context, string, string) (contract.StampedDocument, error)
	ListSigningRecords(context.Context, string, int) ([]contract.SigningRecord, error)
	GetSigningRecord(context.Context, string, string) (contract.SigningRecord, error)
	SaveSigningShipment(context.Context, string, string, string, contract.SigningShipment) error
	MarkSigningReceived(context.Context, string, string, string) error
	RecordSigningReminder(context.Context, string, string, string) error
	ConfirmSigning(context.Context, string, string, string, contract.SigningConfirmation) error
	ListContractLifecycle(context.Context, string, string) ([]contract.LifecycleEvent, error)
	ContractDashboard(context.Context, string, string, time.Time, int) (contract.Dashboard, error)
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

type Service struct {
	Repo             Repository
	Templates        TemplateRepository
	Temporal         client.Client
	TaskQueue        string
	NodeTimeout      time.Duration
	ReminderInterval time.Duration
}

func (s *Service) ListApprovedContracts(ctx context.Context, actor Principal, limit int) ([]contract.Contract, error) {
	if !actor.Has("contract.approved.read") {
		return nil, ErrForbidden
	}
	return s.Repo.ListApprovedContracts(ctx, actor.TenantID, limit)
}

func (s *Service) GetApprovedContract(ctx context.Context, actor Principal, id, permission string) (contract.Contract, error) {
	if !actor.Has(permission) {
		return contract.Contract{}, ErrForbidden
	}
	found, err := s.Repo.GetContract(ctx, actor.TenantID, id)
	if err != nil {
		return found, err
	}
	if !found.Status.ApprovalPassed() {
		return contract.Contract{}, ErrForbidden
	}
	return found, nil
}

func (s *Service) SaveStampedDocument(ctx context.Context, actor Principal, id, filename string, document []byte) error {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.stamped_pdf.upload"); err != nil {
		return err
	}
	return s.Repo.SaveStampedDocument(ctx, actor.TenantID, contract.StampedDocument{ContractID: id, OriginalFilename: filename, Document: document, UploadedAt: time.Now().UTC(), UploadedBy: actor.UserID})
}

func (s *Service) GetStampedDocument(ctx context.Context, actor Principal, id string) (contract.StampedDocument, error) {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.document.download"); err != nil {
		return contract.StampedDocument{}, err
	}
	return s.Repo.GetStampedDocument(ctx, actor.TenantID, id)
}

func (s *Service) ListSigningRecords(ctx context.Context, actor Principal, limit int) ([]contract.SigningRecord, error) {
	if !actor.Has("contract.approved.read") {
		return nil, ErrForbidden
	}
	return s.Repo.ListSigningRecords(ctx, actor.TenantID, limit)
}

func (s *Service) GetSigningRecord(ctx context.Context, actor Principal, id string) (contract.SigningRecord, error) {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.approved.read"); err != nil {
		return contract.SigningRecord{}, err
	}
	return s.Repo.GetSigningRecord(ctx, actor.TenantID, id)
}

func (s *Service) SaveSigningShipment(ctx context.Context, actor Principal, id string, shipment contract.SigningShipment) error {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	current, err := s.Repo.GetSigningRecord(ctx, actor.TenantID, id)
	if err != nil {
		return err
	}
	if current.Status == contract.SigningPendingReview || current.Status == contract.SigningCompleted {
		return apperrors.ErrStateConflict
	}
	return s.Repo.SaveSigningShipment(ctx, actor.TenantID, id, actor.UserID, shipment)
}

func (s *Service) MarkSigningReceived(ctx context.Context, actor Principal, id string) error {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	return s.Repo.MarkSigningReceived(ctx, actor.TenantID, id, actor.UserID)
}

func (s *Service) RecordSigningReminder(ctx context.Context, actor Principal, id string) error {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	return s.Repo.RecordSigningReminder(ctx, actor.TenantID, id, actor.UserID)
}

func (s *Service) ConfirmSigning(ctx context.Context, actor Principal, id string, confirmation contract.SigningConfirmation) error {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	if !confirmation.SealVerified || !confirmation.SignatureVerified || confirmation.SignedAt.IsZero() {
		return ErrValidation
	}
	return s.Repo.ConfirmSigning(ctx, actor.TenantID, id, actor.UserID, confirmation)
}

type UserReference struct {
	UserID      string   `json:"user_id"`
	DisplayName string   `json:"display_name"`
	Roles       []string `json:"roles"`
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
	if c.TemplateID != "" {
		rendered, normalizedValues, err := s.renderTemplate(ctx, actor, c.TemplateID, c.TemplateValues)
		if err != nil {
			return c, err
		}
		c.TemplateValues = normalizedValues
		c.Document = rendered
		c.Content, err = docx.PlainText(rendered)
		if err != nil {
			return c, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		item, err := s.Templates.GetTemplate(ctx, actor.TenantID, c.TemplateID)
		if err != nil {
			return c, err
		}
		c.NumberFormat = item.NumberFormat
	}
	if c.Title == "" || c.Type == "" || c.TemplateID == "" || c.AmountMinor < 0 || c.Content == "" && len(c.Document) == 0 || !validServiceItems(c.ServiceItems) || (c.OpportunityID == "") != (c.CRMCustomerID == 0) {
		return c, ErrValidation
	}
	c.ServiceType = c.ServiceItems[0].ServiceType
	c.Systems = flattenedSystems(c.ServiceItems)
	if c.StartDate != nil && c.EndDate != nil && c.StartDate.After(*c.EndDate) {
		return c, ErrValidation
	}
	if c.Currency == "" {
		c.Currency = "CNY"
	}
	if c.NumberFormat == "" {
		c.NumberFormat = contracttemplate.DefaultNumberFormat
	}
	c.ID, c.TenantID, c.OwnerUserID, c.OwnerDisplayName, c.Status = ulid.Make().String(), actor.TenantID, actor.UserID, actor.DisplayName, contract.StatusDraft
	hashSource := []byte(c.Content)
	if len(c.Document) > 0 {
		hashSource = c.Document
	}
	hash := sha256.Sum256(hashSource)
	c.ContentHash = hex.EncodeToString(hash[:])
	if err := s.Repo.CreateContract(ctx, c, actor.UserID); err != nil {
		return c, err
	}
	c.Version = 1
	return c, nil
}

func validSystems(items []contract.SystemInfo) bool {
	if len(items) > 15 {
		return false
	}
	levels := map[string]bool{"一级": true, "二级": true, "三级": true, "四级": true}
	for _, item := range items {
		if item.Name == "" || !levels[item.Level] {
			return false
		}
	}
	return true
}

func validServiceItems(items []contract.ServiceItem) bool {
	if len(items) == 0 || len(items) > 20 {
		return false
	}
	for _, item := range items {
		if item.ServiceType == "" || !validSystems(item.Systems) {
			return false
		}
	}
	return true
}

func flattenedSystems(items []contract.ServiceItem) []contract.SystemInfo {
	result := make([]contract.SystemInfo, 0)
	for _, item := range items {
		result = append(result, item.Systems...)
	}
	return result
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
	if err := s.resolveNodes(actor, nodes); err != nil {
		return StartResult{}, err
	}
	approvalID := ulid.Make().String()
	workflowID := fmt.Sprintf("contract-approval:%s:%s:v%d", actor.TenantID, contractID, c.Version)
	in := workflows.ContractApprovalInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: contractID, ContractVersion: c.Version, ApplicantUserID: actor.UserID, ApplicantDisplayName: actor.DisplayName, ContentHash: c.ContentHash, RuleID: ruleID, RuleVersion: ruleVersion, Nodes: nodes, DefaultNodeTimeout: s.NodeTimeout, ReminderInterval: s.ReminderInterval}
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
	admins := approversForRole(actor.UserDirectory, "admin")
	if len(admins) == 0 {
		return StartResult{}, fmt.Errorf("no active platform user has role admin")
	}
	approvalID := ulid.Make().String()
	workflowID := fmt.Sprintf("status-change:%s:%s:v%d", actor.TenantID, contractID, version)
	in := workflows.StatusChangeInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: contractID, ContractVersion: version, ApplicantUserID: actor.UserID, ApplicantDisplayName: actor.DisplayName, FromStatus: c.Status, TargetStatus: target, Reason: reason, AdminUserIDs: admins, Timeout: s.NodeTimeout}
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
	command.CommandID, command.ActorUserID, command.ActorDisplayName, command.OccurredAt = ulid.Make().String(), actor.UserID, actor.DisplayName, time.Now().UTC()
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
	if c.OwnerUserID != actor.UserID && !hasRole(actor, "admin") {
		return contract.Contract{}, ErrForbidden
	}
	return c, nil
}

func (s *Service) ListContractLifecycle(ctx context.Context, actor Principal, id string) ([]contract.LifecycleEvent, error) {
	if _, err := s.GetContract(ctx, actor, id); err != nil {
		return nil, err
	}
	return s.Repo.ListContractLifecycle(ctx, actor.TenantID, id)
}

func (s *Service) ListContracts(ctx context.Context, actor Principal, _ string, status string, limit int) ([]contract.Contract, error) {
	if !actor.Has("contract.read") {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	ownerUserID := actor.UserID
	if hasRole(actor, "admin") {
		ownerUserID = ""
	}
	return s.Repo.ListContracts(ctx, actor.TenantID, ownerUserID, status, limit)
}

func (s *Service) ContractDashboard(ctx context.Context, actor Principal) (contract.Dashboard, error) {
	if !actor.Has("contract.read") {
		return contract.Dashboard{}, ErrForbidden
	}
	if s.Repo == nil {
		return contract.Dashboard{}, fmt.Errorf("contract repository is not configured")
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	ownerUserID := actor.UserID
	if hasRole(actor, "admin") {
		ownerUserID = ""
	}
	return s.Repo.ContractDashboard(ctx, actor.TenantID, ownerUserID, today, 200)
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

func (s *Service) resolveNodes(actor Principal, nodes []approval.Node) error {
	for i := range nodes {
		// Assignees are always rebuilt from the platform's current effective roles. Persisted
		// rule assignee IDs are intentionally ignored so personnel changes do not require
		// editing approval rules.
		nodes[i].AssigneeIDs = approversForRole(actor.UserDirectory, nodes[i].RoleCode)
		nodes[i].AssigneeIDs = unique(nodes[i].AssigneeIDs)
		nodes[i].Countersign = approval.CountersignAny
		if nodes[i].ID == "" {
			nodes[i].ID = fmt.Sprintf("node-%d", i+1)
		}
		if len(nodes[i].AssigneeIDs) == 0 {
			return fmt.Errorf("no active platform user has role %s", nodes[i].RoleCode)
		}
	}
	return nil
}

func defaultNodes() []approval.Node {
	return []approval.Node{
		{ID: "sales-director", Name: "销售总监审批", RoleCode: "sales_director", Countersign: approval.CountersignAny},
		{ID: "tech-director", Name: "技术总监审批", RoleCode: "tech_director", Countersign: approval.CountersignAny},
		{ID: "finance-director", Name: "财务总监审批", RoleCode: "finance_director", Countersign: approval.CountersignAny},
	}
}

func approversForRole(directory []UserReference, roleCode string) []string {
	result := make([]string, 0)
	for _, user := range directory {
		for _, role := range user.Roles {
			if role == roleCode {
				result = append(result, user.UserID)
				break
			}
		}
	}
	return unique(result)
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
