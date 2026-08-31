package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
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
	// Subject is the issuer-native OIDC subject (Keycloak user id). UserID and
	// IdentityID remain the platform identity id used by business ownership and
	// audit rules.
	Subject                                string
	TenantID, UserID, IdentityID, PersonID string
	DisplayName, UserName, Email           string
	Roles                                  []string
	Permissions                            map[string]bool
	DataScopes                             []DataScope
	PermissionScopes                       map[string]contract.ScopeFilter
	AuthorizationRevision                  uint64
	CatalogVersion                         string
}

func (p Principal) Has(permission string) bool { return p.Permissions[permission] }

type DataScope struct {
	RoleCode        string `json:"role_code"`
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	EnvironmentCode string `json:"environment_code"`
}

func (p Principal) Scope(permission string) (contract.ScopeFilter, bool) {
	if !p.Has(permission) {
		return contract.ScopeFilter{}, false
	}
	filter, ok := p.PermissionScopes[permission]
	if !ok {
		return contract.ScopeFilter{}, false
	}
	filter.TenantID = p.TenantID
	filter.IdentityID = p.IdentityID
	return filter, filter.AllowAll || filter.AllowSelf || len(filter.OrganizationIDs) > 0 || len(filter.ProjectIDs) > 0
}

type Repository interface {
	GetContract(context.Context, string, string) (contract.Contract, error)
	ListContracts(context.Context, string, string, string, string, int) ([]contract.Contract, error)
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

type PersonnelDirectory interface {
	ListEligibleUsers(context.Context, Principal, []string) ([]UserReference, error)
}

type ScopedRepository interface {
	GetContractScoped(context.Context, contract.ScopeFilter, string) (contract.Contract, error)
	ListContractsScoped(context.Context, contract.ScopeFilter, string, string, int) ([]contract.Contract, error)
	ListApprovedContractsScoped(context.Context, contract.ScopeFilter, int) ([]contract.Contract, error)
	ListSigningRecordsScoped(context.Context, contract.ScopeFilter, int) ([]contract.SigningRecord, error)
	GetSigningRecordScoped(context.Context, contract.ScopeFilter, string) (contract.SigningRecord, error)
	ContractDashboardScoped(context.Context, contract.ScopeFilter, time.Time, int) (contract.Dashboard, error)
}

type Service struct {
	Repo                     Repository
	Templates                TemplateRepository
	Temporal                 client.Client
	TaskQueue                string
	NodeTimeout              time.Duration
	ReminderInterval         time.Duration
	Personnel                PersonnelDirectory
	OpportunityLinkNotifier  OpportunityLinkNotifier
	StampedFileGateway       StampedFileGateway
	StampedFileMode          string
	StampedFileApplicationID string
}

// StampedFileGateway 上传并绑定盖章合同；跨系统提交失败时旧 BLOB 仍可回退读取。
type StampedFileGateway interface {
	Upload(context.Context, string, string, string, string, string, io.Reader) (string, error)
	Bind(context.Context, string, string, string, string, string, string) error
	Download(context.Context, string) ([]byte, error)
}

// StampedDocumentGatewayStateRepository 持久化跨系统上传状态，供失败重试和运维对账使用。
type StampedDocumentGatewayStateRepository interface {
	MarkStampedDocumentGatewayPending(context.Context, string, string) error
	MarkStampedDocumentGatewayResult(context.Context, string, string, string, string) error
}

type OpportunityLinkNotifier interface {
	NotifyOpportunityLink(context.Context, OpportunityIntake) error
}

func (s *Service) ListApprovedContracts(ctx context.Context, actor Principal, limit int) ([]contract.Contract, error) {
	filter, ok := s.contractScope(actor, "contract.approved.read")
	if !ok {
		return nil, ErrForbidden
	}
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.ListApprovedContractsScoped(ctx, filter, limit)
	}
	items, err := s.Repo.ListApprovedContracts(ctx, actor.TenantID, limit)
	return filterContracts(items, filter), err
}

func (s *Service) GetApprovedContract(ctx context.Context, actor Principal, id, permission string) (contract.Contract, error) {
	filter, ok := s.contractScope(actor, permission)
	if !ok {
		return contract.Contract{}, ErrForbidden
	}
	found, err := s.getContractScoped(ctx, filter, id)
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
	if err := s.Repo.SaveStampedDocument(ctx, actor.TenantID, contract.StampedDocument{ContractID: id, OriginalFilename: filename, Document: document, UploadedAt: time.Now().UTC(), UploadedBy: actor.UserID}); err != nil {
		return err
	}
	if s.StampedFileGateway == nil || strings.EqualFold(strings.TrimSpace(s.StampedFileMode), "legacy") {
		return nil
	}
	if stateRepo, ok := s.Repo.(StampedDocumentGatewayStateRepository); ok {
		if err := stateRepo.MarkStampedDocumentGatewayPending(ctx, actor.TenantID, id); err != nil {
			return fmt.Errorf("mark file gateway upload pending: %w", err)
		}
	}
	documentHash := sha256.Sum256(document)
	requestID := fmt.Sprintf("contract-stamped-%s-%x", id, documentHash[:8])
	fileID, err := s.StampedFileGateway.Upload(ctx, requestID, s.StampedFileApplicationID, "CONTRACT_STAMPED_PDF", filename, "application/pdf", bytes.NewReader(document))
	if err == nil {
		err = s.StampedFileGateway.Bind(ctx, s.StampedFileApplicationID, fileID, "contract", id, "STAMPED_PDF", filename)
	}
	if err != nil {
		if stateRepo, ok := s.Repo.(StampedDocumentGatewayStateRepository); ok {
			_ = stateRepo.MarkStampedDocumentGatewayResult(ctx, actor.TenantID, id, "FAILED", err.Error())
		}
		if strings.EqualFold(strings.TrimSpace(s.StampedFileMode), "dual") {
			// 双写灰度期间旧 BLOB 已持久化，网关失败只留下可重试状态，不阻断合同业务。
			return nil
		}
		return fmt.Errorf("file gateway upload pending; legacy document retained: %w", err)
	}
	if stateRepo, ok := s.Repo.(StampedDocumentGatewayStateRepository); ok {
		if err := stateRepo.MarkStampedDocumentGatewayResult(ctx, actor.TenantID, id, "READY", fileID); err != nil {
			return fmt.Errorf("mark file gateway upload ready: %w", err)
		}
	}
	return nil
}

func (s *Service) GetStampedDocument(ctx context.Context, actor Principal, id string) (contract.StampedDocument, error) {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.document.download"); err != nil {
		return contract.StampedDocument{}, err
	}
	document, err := s.Repo.GetStampedDocument(ctx, actor.TenantID, id)
	if err != nil {
		return contract.StampedDocument{}, err
	}
	mode := strings.ToLower(strings.TrimSpace(s.StampedFileMode))
	if s.StampedFileGateway == nil || mode == "legacy" || document.PlatformFileID == "" || !strings.EqualFold(document.FileGatewayState, "READY") {
		return document, nil
	}
	content, gatewayErr := s.StampedFileGateway.Download(ctx, document.PlatformFileID)
	if gatewayErr == nil {
		document.Document = content
		return document, nil
	}
	if mode == "required" {
		return contract.StampedDocument{}, fmt.Errorf("download stamped document from file gateway: %w", gatewayErr)
	}
	// 双写阶段只有在网关读取失败时回退历史 BLOB；网关成功后不再从数据库加载另一份内容。
	return document, nil
}

func (s *Service) ListSigningRecords(ctx context.Context, actor Principal, limit int) ([]contract.SigningRecord, error) {
	filter, ok := actor.Scope("contract.approved.read")
	if !ok {
		return nil, ErrForbidden
	}
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.ListSigningRecordsScoped(ctx, filter, limit)
	}
	items, err := s.Repo.ListSigningRecords(ctx, actor.TenantID, limit)
	result := make([]contract.SigningRecord, 0, len(items))
	for _, item := range items {
		if canAccessContract(filter, item.Contract) {
			result = append(result, item)
		}
	}
	return result, err
}

func (s *Service) GetSigningRecord(ctx context.Context, actor Principal, id string) (contract.SigningRecord, error) {
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.approved.read"); err != nil {
		return contract.SigningRecord{}, err
	}
	filter, _ := actor.Scope("contract.approved.read")
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.GetSigningRecordScoped(ctx, filter, id)
	}
	item, err := s.Repo.GetSigningRecord(ctx, actor.TenantID, id)
	if err == nil && !canAccessContract(filter, item.Contract) {
		return contract.SigningRecord{}, ErrForbidden
	}
	return item, err
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
	filter, ok := actor.Scope("contract.create")
	if !ok {
		return c, ErrForbidden
	}
	if filter.AllowSelf {
		c.OwnerIdentityID = actor.IdentityID
	}
	if !filter.AllowAll && !filter.AllowSelf && !containsString(filter.OrganizationIDs, c.OwnerOrgID) && !containsString(filter.ProjectIDs, c.ProjectID) {
		return c, ErrForbidden
	}
	if c.OwnerOrgID != "" && !filter.AllowAll && !containsString(filter.OrganizationIDs, c.OwnerOrgID) {
		return c, ErrForbidden
	}
	if c.ProjectID != "" && !filter.AllowAll && !containsString(filter.ProjectIDs, c.ProjectID) {
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
	if c.OwnerIdentityID == "" && filter.AllowAll {
		c.OwnerIdentityID = actor.IdentityID
	}
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
		mode := strings.ToUpper(strings.TrimSpace(item.TestMode))
		if item.ServiceType == "" || !validSystems(item.Systems) || (mode != "" && mode != "STANDARD" && mode != "PENETRATION") {
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
	filter, ok := actor.Scope("contract.create")
	if !ok {
		return StartResult{}, ErrForbidden
	}
	c, err := s.getContractScoped(ctx, filter, contractID)
	if err != nil {
		return StartResult{}, err
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
	if err := s.resolveNodes(ctx, actor, nodes); err != nil {
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
	filter, ok := actor.Scope("contract.edit")
	if !ok {
		return StartResult{}, ErrForbidden
	}
	if reason == "" {
		return StartResult{}, ErrValidation
	}
	c, err := s.getContractScoped(ctx, filter, contractID)
	if err != nil {
		return StartResult{}, err
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
	directory, directoryErr := s.loadPersonnel(ctx, actor, []string{"admin"})
	if directoryErr != nil {
		return StartResult{}, directoryErr
	}
	admins := approversForRole(directory, "admin")
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

func (s *Service) Command(ctx context.Context, actor Principal, approvalID string, command workflows.ApprovalCommand) (string, error) {
	meta, _, err := s.queryApprovalState(ctx, actor, approvalID)
	if err != nil {
		return "", err
	}
	if meta.Status != approval.StatusRunning {
		return "", apperrors.ErrStateConflict
	}
	switch command.Action {
	case workflows.ActionWithdraw:
		if meta.ApplicantUserID != actor.UserID {
			return "", ErrForbidden
		}
	case workflows.ActionUrge:
		if meta.ApplicantUserID != actor.UserID && !actor.Has("approval.manage") {
			return "", ErrForbidden
		}
	case workflows.ActionComment:
		if !actor.Has("approval.view") && meta.ApplicantUserID != actor.UserID {
			return "", ErrForbidden
		}
	default:
		if !actor.Has("approval.process") {
			return "", ErrForbidden
		}
	}
	if command.Action == workflows.ActionAddSign {
		directory, directoryErr := s.loadPersonnel(ctx, actor, []string{"admin", "sales_director", "tech_director", "finance_director"})
		if directoryErr != nil {
			return "", directoryErr
		}
		if err := validateApprovalProcessTargets(directory, command.TargetUserIDs); err != nil {
			return "", err
		}
	}
	// Approval-rule nodes assign every active user of a director role. Carry the
	// or-sign policy on new commands so running workflows do not need historical
	// decisions rewritten when this behavior is introduced.
	command.RoleNodeOrSign = command.Action == workflows.ActionApprove
	command.CommandID, command.ActorUserID, command.ActorDisplayName, command.OccurredAt = ulid.Make().String(), actor.UserID, actor.DisplayName, time.Now().UTC()
	if err := s.Temporal.SignalWorkflow(ctx, meta.WorkflowID, meta.RunID, workflows.CommandSignalName, command); err != nil {
		return "", fmt.Errorf("%w: signal approval workflow %s: %v", ErrApprovalWorkflowUnavailable, meta.WorkflowID, err)
	}
	return command.CommandID, nil
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
	contractSnapshot, err := s.getApprovalContract(ctx, actor, meta.ContractID)
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
	if _, err := s.getApprovalContract(ctx, actor, meta.ContractID); err != nil {
		return approval.Meta{}, workflows.ApprovalState{}, err
	}
	if !actor.Has("approval.view") && !actor.Has("approval.process") && meta.ApplicantUserID != actor.UserID {
		return approval.Meta{}, workflows.ApprovalState{}, ErrForbidden
	}
	encoded, err := s.Temporal.QueryWorkflow(ctx, meta.WorkflowID, meta.RunID, workflows.StateQueryName)
	if err != nil {
		return approval.Meta{}, workflows.ApprovalState{}, fmt.Errorf("%w: query approval workflow %s: %v", ErrApprovalWorkflowUnavailable, meta.WorkflowID, err)
	}
	var state workflows.ApprovalState
	if err := encoded.Get(&state); err != nil {
		return approval.Meta{}, workflows.ApprovalState{}, fmt.Errorf("%w: decode approval workflow %s state: %v", ErrApprovalWorkflowUnavailable, meta.WorkflowID, err)
	}
	return meta, state, nil
}

func (s *Service) getApprovalContract(ctx context.Context, actor Principal, contractID string) (contract.Contract, error) {
	filter, ok := actor.Scope("contract.read")
	if !ok {
		return contract.Contract{}, ErrForbidden
	}
	return s.getContractScoped(ctx, filter, contractID)
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
	filter, ok := s.contractScope(actor, "contract.read")
	if !ok {
		return contract.Contract{}, ErrForbidden
	}
	return s.getContractScoped(ctx, filter, id)
}

func (s *Service) ListContractLifecycle(ctx context.Context, actor Principal, id string) ([]contract.LifecycleEvent, error) {
	if _, err := s.GetContract(ctx, actor, id); err != nil {
		return nil, err
	}
	return s.Repo.ListContractLifecycle(ctx, actor.TenantID, id)
}

func (s *Service) ListContracts(ctx context.Context, actor Principal, keyword string, status string, limit int) ([]contract.Contract, error) {
	filter, ok := s.contractScope(actor, "contract.read")
	if !ok {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.ListContractsScoped(ctx, filter, keyword, status, limit)
	}
	if !filter.AllowAll && !filter.AllowSelf && len(filter.OrganizationIDs) == 0 && len(filter.ProjectIDs) == 0 {
		return nil, ErrForbidden
	}
	ownerUserID := ""
	if filter.AllowSelf {
		ownerUserID = actor.UserID
	}
	items, err := s.Repo.ListContracts(ctx, actor.TenantID, keyword, ownerUserID, status, limit)
	return filterContracts(items, filter), err
}

func (s *Service) ContractDashboard(ctx context.Context, actor Principal) (contract.Dashboard, error) {
	filter, ok := s.contractScope(actor, "contract.read")
	if !ok {
		return contract.Dashboard{}, ErrForbidden
	}
	if s.Repo == nil {
		return contract.Dashboard{}, fmt.Errorf("contract repository is not configured")
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var (
		dashboard contract.Dashboard
		err       error
	)
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		dashboard, err = scoped.ContractDashboardScoped(ctx, filter, today, 200)
	} else if filter.AllowAll {
		dashboard, err = s.Repo.ContractDashboard(ctx, actor.TenantID, "", today, 200)
	} else if filter.AllowSelf {
		dashboard, err = s.Repo.ContractDashboard(ctx, actor.TenantID, actor.UserID, today, 200)
	} else {
		return contract.Dashboard{}, ErrForbidden
	}
	if err != nil {
		return contract.Dashboard{}, err
	}
	// internal/dashboard 的机器消费方必须能够校验返回数据与请求租户一致，
	// 因此租户标识由应用层基于已认证主体统一写入，不能信任仓储返回值。
	dashboard.TenantID = actor.TenantID
	return dashboard, nil
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

func (s *Service) resolveNodes(ctx context.Context, actor Principal, nodes []approval.Node) error {
	roleCodes := make([]string, 0, len(nodes))
	for _, node := range nodes {
		roleCodes = append(roleCodes, node.RoleCode)
	}
	directory, err := s.loadPersonnel(ctx, actor, unique(roleCodes))
	if err != nil {
		return err
	}
	for i := range nodes {
		// Assignees are always rebuilt from the platform's current effective roles. Persisted
		// rule assignee IDs are intentionally ignored so personnel changes do not require
		// editing approval rules.
		nodes[i].AssigneeIDs = approversForRole(directory, nodes[i].RoleCode)
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

func (s *Service) loadPersonnel(ctx context.Context, actor Principal, roleCodes []string) ([]UserReference, error) {
	if s.Personnel == nil {
		return nil, ErrPersonnelDirectoryUnavailable
	}
	directory, err := s.Personnel.ListEligibleUsers(ctx, actor, roleCodes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPersonnelDirectoryUnavailable, err)
	}
	return directory, nil
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

func validateApprovalProcessTargets(directory []UserReference, targetUserIDs []string) error {
	targets := unique(targetUserIDs)
	if len(targets) == 0 {
		return ErrApprovalTargetForbidden
	}
	eligible := make(map[string]bool, len(directory))
	for _, user := range directory {
		for _, role := range user.Roles {
			if roleCanProcessApproval(role) {
				eligible[user.UserID] = true
				break
			}
		}
	}
	for _, target := range targets {
		if !eligible[target] {
			return ErrApprovalTargetForbidden
		}
	}
	return nil
}

func roleCanProcessApproval(roleCode string) bool {
	switch roleCode {
	case "admin", "sales_director", "tech_director", "finance_director":
		return true
	default:
		return false
	}
}

func (s *Service) taskQueue() string {
	if s.TaskQueue == "" {
		return workflows.TaskQueue
	}
	return s.TaskQueue
}

func (s *Service) getContractScoped(ctx context.Context, filter contract.ScopeFilter, id string) (contract.Contract, error) {
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.GetContractScoped(ctx, filter, id)
	}
	item, err := s.Repo.GetContract(ctx, filter.TenantID, id)
	if err != nil {
		return item, err
	}
	if !canAccessContract(filter, item) {
		return contract.Contract{}, ErrForbidden
	}
	return item, nil
}

func (s *Service) contractScope(actor Principal, permission string) (contract.ScopeFilter, bool) {
	filter, ok := actor.Scope(permission)
	if !ok {
		return contract.ScopeFilter{}, false
	}
	// Contract-domain defense in depth: a plain sales user is always limited to
	// contracts owned by the current identity, even if a stale platform claim
	// accidentally carries an APPLICATION/TENANT-wide scope. Elevated contract
	// roles retain their explicitly issued scope for approval work.
	if permission == "contract.read" && salesOnly(actor.Roles) {
		filter.AllowAll = false
		filter.AllowSelf = true
		filter.OrganizationIDs = nil
		filter.ProjectIDs = nil
	}
	return filter, true
}

func salesOnly(roles []string) bool {
	found := false
	for _, role := range roles {
		switch role {
		case "admin", "sales_director", "tech_director", "finance_director", "audit_admin":
			return false
		case "sales":
			found = true
		}
	}
	return found
}

func canAccessContract(filter contract.ScopeFilter, item contract.Contract) bool {
	if item.TenantID != filter.TenantID {
		return false
	}
	if filter.AllowAll {
		return true
	}
	return filter.AllowSelf && item.OwnerIdentityID != "" && item.OwnerIdentityID == filter.IdentityID ||
		item.OwnerOrgID != "" && containsString(filter.OrganizationIDs, item.OwnerOrgID) ||
		item.ProjectID != "" && containsString(filter.ProjectIDs, item.ProjectID)
}

func filterContracts(items []contract.Contract, filter contract.ScopeFilter) []contract.Contract {
	result := make([]contract.Contract, 0, len(items))
	for _, item := range items {
		if canAccessContract(filter, item) {
			result = append(result, item)
		}
	}
	return result
}

func containsString(values []string, expected string) bool {
	if expected == "" {
		return false
	}
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
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
	ErrForbidden                     = errors.New("forbidden")
	ErrValidation                    = errors.New("validation failed")
	ErrApprovalTargetForbidden       = errors.New("approval target cannot process contract approvals")
	ErrPersonnelDirectoryUnavailable = errors.New("approval personnel directory is unavailable")
	ErrApprovalWorkflowUnavailable   = errors.New("approval workflow is unavailable")
)
