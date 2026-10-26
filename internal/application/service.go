package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/auditctx"
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
	// Subject 是签发者原生 OIDC 主体（Keycloak 用户 ID）。UserID 和
	// IdentityID 仍为业务归属及审计规则使用的
	// 平台身份 ID。
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

// DocumentAccessRecorder 记录敏感文档访问（下载/预览）。
// 由仓储层实现；缺失实现时调用方必须视为"无法建立审计依据"。
type DocumentAccessRecorder interface {
	RecordDocumentAccess(ctx context.Context, tenantID, contractID, documentKind, actorID string) error
}

type Repository interface {
	GetContract(context.Context, string, string) (contract.Contract, error)
	ListContracts(context.Context, string, string, string, string, int) ([]contract.Contract, error)
	ListApprovedContracts(context.Context, string, int) ([]contract.Contract, error)
	ListApprovedContractReferences(context.Context, string, string, int) ([]contract.Contract, error)
	SaveStampedDocument(context.Context, string, contract.StampedDocumentUpload) (contract.StampedDocument, error)
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
	// GetApprovalSnapshot 返回审批发起时的不可变合同快照；旧审批没有快照时 found=false。
	GetApprovalSnapshot(context.Context, string, string) (contract.Contract, bool, error)
	ListApprovalActions(context.Context, string, string) ([]approval.Action, error)
	ListApprovals(context.Context, string, string, int) ([]approval.Summary, error)
	ListTasks(context.Context, string, string, int) ([]approval.Task, error)
}

type PersonnelDirectory interface {
	ListEligibleUsers(context.Context, Principal, []string) ([]UserReference, error)
}

// ApprovalRecoveryStore 是管理端“工作流执行故障收口”所需的最小持久化能力。
// CompleteApproval 必须是幂等的；恢复流程只在工作流已停止时调用。
type ApprovalRecoveryStore interface {
	GetApprovalRuntimeState(context.Context, string, string) (workflows.ApprovalState, bool, error)
	CompleteApproval(context.Context, workflows.CompleteApprovalActivityInput) error
}

type ScopedRepository interface {
	GetContractScoped(context.Context, contract.ScopeFilter, string) (contract.Contract, error)
	ListContractsScoped(context.Context, contract.ScopeFilter, string, string, int) ([]contract.Contract, error)
	ListApprovedContractsScoped(context.Context, contract.ScopeFilter, int) ([]contract.Contract, error)
	ListApprovedContractReferencesScoped(context.Context, contract.ScopeFilter, string, int) ([]contract.Contract, error)
	ListSigningRecordsScoped(context.Context, contract.ScopeFilter, contract.SigningRecordQuery) (contract.SigningRecordPage, error)
	GetSigningRecordScoped(context.Context, contract.ScopeFilter, string) (contract.SigningRecord, error)
	ContractDashboardScoped(context.Context, contract.ScopeFilter, time.Time, int) (contract.Dashboard, error)
}

type Service struct {
	LicenseGate              LicenseChecker
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
	DetectionCategories      DetectionCategoryDirectory
	CRMContractReferences    CRMContractReferenceDirectory
	// HandoverFreeze 在写路径上阻止把新责任分配给交接中的用户。
	HandoverFreeze HandoverFreezeChecker
}

// StampedFileGateway 上传并绑定盖章合同；跨系统提交失败时旧 BLOB 仍可回退读取。
type StampedFileGateway interface {
	Upload(context.Context, string, string, string, string, string, io.Reader) (string, error)
	Bind(context.Context, string, string, string, string, string, string) error
	Download(context.Context, string) ([]byte, error)
}

// StampedDocumentGatewayStateRepository 持久化跨系统上传状态，供失败重试和运维对账使用。
type StampedDocumentGatewayStateRepository interface {
	// version 与 contentSHA256 共同限定回写目标：并发上传时旧请求不得把
	// READY/FAILED 写到新版本行上。
	MarkStampedDocumentGatewayPending(context.Context, string, string, uint64, string) error
	MarkStampedDocumentGatewayResult(context.Context, string, string, uint64, string, string, string) error
}

type SourceFileGatewayStateRepository interface {
	MarkContractSourceGatewayResult(context.Context, string, string, string, string, string) error
	MarkTemplateGatewayResult(context.Context, string, string, string, string, string) error
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

// ListApprovedContractReferences 是按 ID 稳定排序的集成数据流，
// 用于将合同管理与项目管理中已持久化的项目
// 对账。游标避免 UI 列表 200 行上限，并限制每次跨
// 服务请求规模。
func (s *Service) ListApprovedContractReferences(ctx context.Context, actor Principal, afterID string, limit int) ([]contract.Contract, error) {
	filter, ok := s.contractScope(actor, "contract.approved.read")
	if !ok {
		return nil, ErrForbidden
	}
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.ListApprovedContractReferencesScoped(ctx, filter, afterID, limit)
	}
	if !filter.AllowAll {
		return nil, ErrForbidden
	}
	return s.Repo.ListApprovedContractReferences(ctx, actor.TenantID, afterID, limit)
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

// SaveStampedDocument 上传盖章文件；每次上传都会形成新的不可变版本。
// expectedVersion 为操作者打开/核验的版本，0 表示兼容旧客户端由服务端取当前版本。
func (s *Service) SaveStampedDocument(ctx context.Context, actor Principal, id, filename string, document []byte, expectedVersion uint64) (contract.StampedDocument, error) {
	return s.saveStampedDocument(ctx, actor, id, filename, document, expectedVersion, "")
}

// ReplaceStampedDocument 是已确认签署合同的正式文件更换入口：必须带原因和
// 明确的旧版本号，并重新进入人工核验；历史版本及历史确认记录不会被覆盖。
func (s *Service) ReplaceStampedDocument(ctx context.Context, actor Principal, id, filename string, document []byte, expectedVersion uint64, reason string) (contract.StampedDocument, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return contract.StampedDocument{}, fmt.Errorf("%w: 正式文件更换必须填写原因", ErrValidation)
	}
	if len([]rune(reason)) > 500 {
		return contract.StampedDocument{}, fmt.Errorf("%w: 正式文件更换原因不能超过 500 个字符", ErrValidation)
	}
	if expectedVersion == 0 {
		return contract.StampedDocument{}, fmt.Errorf("%w: 正式文件更换必须携带当前修订号", ErrValidation)
	}
	return s.saveStampedDocument(ctx, actor, id, filename, document, expectedVersion, reason)
}

func (s *Service) saveStampedDocument(ctx context.Context, actor Principal, id, filename string, document []byte, expectedVersion uint64, replacementReason string) (contract.StampedDocument, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.StampedDocument{}, err
	}
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.stamped_pdf.upload"); err != nil {
		return contract.StampedDocument{}, err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.StampedDocument{}, err
	}
	saved, err := s.Repo.SaveStampedDocument(ctx, actor.TenantID, contract.StampedDocumentUpload{
		ContractID: id, OriginalFilename: filename, Document: document,
		UploadedAt: time.Now().UTC(), UploadedBy: actor.UserID,
		ExpectedVersion: expectedVersion, ReplacementReason: replacementReason,
	})
	if err != nil {
		return contract.StampedDocument{}, err
	}
	if s.StampedFileGateway == nil || strings.EqualFold(strings.TrimSpace(s.StampedFileMode), "legacy") {
		return saved, nil
	}
	stateRepo, hasStateRepo := s.Repo.(StampedDocumentGatewayStateRepository)
	if hasStateRepo {
		if err := stateRepo.MarkStampedDocumentGatewayPending(ctx, actor.TenantID, id, saved.Version, saved.ContentSHA256); err != nil {
			return saved, fmt.Errorf("mark file gateway upload pending: %w", err)
		}
	}
	documentHash := sha256.Sum256(document)
	// 请求 ID 绑定版本与内容哈希：同一版本重试可复用，旧版本请求不会覆盖新版本。
	requestID := fmt.Sprintf("contract-stamped-%s-v%d-%x", id, saved.Version, documentHash[:8])
	fileID, uploadErr := s.StampedFileGateway.Upload(ctx, requestID, s.StampedFileApplicationID, "CONTRACT_STAMPED_PDF", filename, "application/pdf", bytes.NewReader(document))
	if uploadErr == nil {
		uploadErr = s.StampedFileGateway.Bind(ctx, s.StampedFileApplicationID, fileID, "contract", id, "STAMPED_PDF", filename)
	}
	if uploadErr != nil {
		if hasStateRepo {
			_ = stateRepo.MarkStampedDocumentGatewayResult(ctx, actor.TenantID, id, saved.Version, saved.ContentSHA256, "FAILED", uploadErr.Error())
		}
		if strings.EqualFold(strings.TrimSpace(s.StampedFileMode), "dual") {
			// 双写灰度期间 BLOB 已按版本持久化，网关失败只留下可重试状态，不阻断合同业务。
			return saved, nil
		}
		return saved, fmt.Errorf("file gateway upload pending; legacy document retained: %w", uploadErr)
	}
	if hasStateRepo {
		if err := stateRepo.MarkStampedDocumentGatewayResult(ctx, actor.TenantID, id, saved.Version, saved.ContentSHA256, "READY", fileID); err != nil {
			return saved, fmt.Errorf("mark file gateway upload ready: %w", err)
		}
	}
	saved.PlatformFileID, saved.FileGatewayState = fileID, "READY"
	return saved, nil
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
		// 下载内容必须与台账 SHA 一致；否则说明网关指向了另一份文件，不能静默返回。
		if digest := sha256.Sum256(content); !strings.EqualFold(hex.EncodeToString(digest[:]), document.ContentSHA256) {
			gatewayErr = fmt.Errorf("file gateway content SHA-256 does not match stamped document version %d", document.Version)
		} else {
			document.Document = content
			return document, nil
		}
	}
	if mode == "required" {
		return contract.StampedDocument{}, fmt.Errorf("download stamped document from file gateway: %w", gatewayErr)
	}
	// 双写回退只返回数据库 BLOB；其内容也按同一哈希校验，避免把不一致内容当成正式文件。
	if digest := sha256.Sum256(document.Document); document.ContentSHA256 != "" && !strings.EqualFold(hex.EncodeToString(digest[:]), document.ContentSHA256) {
		return contract.StampedDocument{}, fmt.Errorf("stamped document BLOB SHA-256 mismatch for version %d", document.Version)
	}
	return document, nil
}

func (s *Service) ListSigningRecords(ctx context.Context, actor Principal, query contract.SigningRecordQuery) (contract.SigningRecordPage, error) {
	filter, ok := actor.Scope("contract.approved.read")
	if !ok {
		return contract.SigningRecordPage{}, ErrForbidden
	}
	if query.Limit <= 0 || query.Limit > 200 {
		query.Limit = 50
	}
	if query.Offset < 0 {
		query.Offset = 0
	}
	if scoped, ok := s.Repo.(ScopedRepository); ok {
		return scoped.ListSigningRecordsScoped(ctx, filter, query)
	}
	// 内存替身/旧仓储的兼容路径：先取上限集合，再按同一条件筛选分页。
	items, err := s.Repo.ListSigningRecords(ctx, actor.TenantID, 200)
	if err != nil {
		return contract.SigningRecordPage{}, err
	}
	filtered := make([]contract.SigningRecord, 0, len(items))
	for _, item := range items {
		if canAccessContract(filter, item.Contract) && signingRecordMatches(item, query) {
			filtered = append(filtered, item)
		}
	}
	stats := signingStatsOf(filtered)
	total := int64(len(filtered))
	offset := query.Offset
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := offset + query.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	return contract.SigningRecordPage{
		Items: filtered[offset:end], Total: total, Offset: query.Offset, Limit: query.Limit,
		HasMore: int64(end) < total, Stats: stats,
	}, nil
}

func signingRecordMatches(item contract.SigningRecord, query contract.SigningRecordQuery) bool {
	keyword := strings.ToLower(strings.TrimSpace(query.Keyword))
	if keyword != "" {
		haystack := strings.ToLower(strings.Join([]string{item.Contract.Number, item.Contract.Title, item.Contract.CustomerName, item.CourierNumber}, " "))
		if !strings.Contains(haystack, keyword) {
			return false
		}
	}
	if method := strings.TrimSpace(query.Method); method != "" && item.Method != method {
		return false
	}
	switch strings.TrimSpace(query.Status) {
	case "":
		return true
	case "completed", "pending_shipment", "in_return", "pending_review":
		return string(item.Status) == strings.TrimSpace(query.Status)
	case "expired":
		return item.Status != contract.SigningCompleted && item.Contract.EndDate != nil && item.Contract.EndDate.Before(time.Now().UTC())
	default:
		return false
	}
}

func signingStatsOf(items []contract.SigningRecord) contract.SigningStats {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	stats := contract.SigningStats{Total: int64(len(items))}
	for _, item := range items {
		expired := item.Status != contract.SigningCompleted && item.Contract.EndDate != nil && item.Contract.EndDate.Before(now)
		if item.Status == contract.SigningCompleted {
			stats.Completed++
			if item.ConfirmedAt != nil && !item.ConfirmedAt.Before(monthStart) && item.ConfirmedAt.Before(nextMonth) {
				stats.Monthly++
			}
		} else if !expired {
			stats.Processing++
		}
		if expired {
			stats.Expired++
		}
	}
	return stats
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
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	// 收件人手机号长度上限（SEC-D7）：列加密密文 = 前缀 + nonce + GCM tag + Base64，
	// 只有 ≤MaxSigningPhonePlaintextLen 个字符的明文才能稳定存入 000016 迁移的
	// VARCHAR(80)；超长按参数不合法拒绝（422），而不是让 SQL 报"数据过长"。
	if phone := strings.TrimSpace(shipment.RecipientPhone); phone == "" || len([]rune(phone)) > contract.MaxSigningPhonePlaintextLen {
		return fmt.Errorf("%w: 收件人手机号必须为 1~%d 个字符", ErrValidation, contract.MaxSigningPhonePlaintextLen)
	}
	if len([]rune(strings.TrimSpace(shipment.CourierNumber))) > 120 ||
		len([]rune(strings.TrimSpace(shipment.RecipientName))) > 120 ||
		len([]rune(strings.TrimSpace(shipment.RecipientAddress))) > 500 {
		return fmt.Errorf("%w: 快递单号/收件人/地址超出长度限制", ErrValidation)
	}
	current, err := s.Repo.GetSigningRecord(ctx, actor.TenantID, id)
	if err != nil {
		return err
	}
	if current.Status == contract.SigningPendingReview || current.Status == contract.SigningCompleted {
		return apperrors.ErrStateConflict
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	return s.Repo.SaveSigningShipment(ctx, actor.TenantID, id, actor.UserID, shipment)
}

func (s *Service) MarkSigningReceived(ctx context.Context, actor Principal, id string) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	return s.Repo.MarkSigningReceived(ctx, actor.TenantID, id, actor.UserID)
}

func (s *Service) RecordSigningReminder(ctx context.Context, actor Principal, id string) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	return s.Repo.RecordSigningReminder(ctx, actor.TenantID, id, actor.UserID)
}

func (s *Service) ConfirmSigning(ctx context.Context, actor Principal, id string, confirmation contract.SigningConfirmation) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if _, err := s.GetApprovedContract(ctx, actor, id, "contract.signing.manage"); err != nil {
		return err
	}
	if !confirmation.SealVerified || !confirmation.SignatureVerified || confirmation.SignedAt.IsZero() ||
		confirmation.DocumentVersion == 0 || strings.TrimSpace(confirmation.ContentSHA256) == "" {
		return ErrValidation
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
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
	// SnapshotAvailable=false 表示部署前旧审批没有提交快照，Contract 是当前合同
	// 的降级展示；页面必须明确提示，不能把它当成历史提交内容。
	SnapshotAvailable bool `json:"snapshot_available"`
	// StateUnavailable=true 表示工作流历史已无法查询
	// （Temporal 状态过期或不可用），但不可变提交快照
	// 仍然可用，必须保持可查看。
	StateUnavailable bool `json:"state_unavailable"`
}

func (s *Service) CreateContract(ctx context.Context, actor Principal, c contract.Contract) (contract.Contract, error) {
	return s.createContract(ctx, actor, c, false)
}

// CreateExternalContract 从调用方提供的 DOCX 创建草稿，
// 不引入第二套工作流或持久化模型。文档冻结在模板合同
// 共用的 rendered_document/content/content_hash 字段中，
// 确保预览、审批和导出使用唯一权威来源。
func (s *Service) CreateExternalContract(ctx context.Context, actor Principal, c contract.Contract) (contract.Contract, error) {
	if _, ok := actor.Scope("contract.create"); !ok {
		return c, ErrForbidden
	}
	if c.TemplateID != "" || len(c.TemplateValues) != 0 || len(c.Document) == 0 {
		return c, ErrValidation
	}
	if err := docx.ValidateExternalDocument(c.Document); err != nil {
		return c, fmt.Errorf("%w: 外部合同 DOCX 解析失败：%v", ErrValidation, err)
	}
	content, err := docx.PlainText(c.Document)
	if err != nil || strings.TrimSpace(content) == "" {
		return c, fmt.Errorf("%w: 外部合同 DOCX 正文不能为空", ErrValidation)
	}
	c.Content = content
	return s.createContract(ctx, actor, c, true)
}

func (s *Service) createContract(ctx context.Context, actor Principal, c contract.Contract, external bool) (contract.Contract, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.Contract{}, err
	}
	c, err := s.prepareContract(ctx, actor, c, external, "contract.create")
	if err != nil {
		return c, err
	}
	// 交接冻结：新建合同的责任人尚未在 prepareContract 中赋值，真正的负责人是当前
	// 操作者（persistNewContract 会写入 actor.UserID）。检查真实责任主体，空值不能放行。
	if err := s.EnsureOwnerNotFrozen(ctx, actor.TenantID, actor.UserID); err != nil {
		return c, err
	}
	filter, _ := actor.Scope("contract.create")
	return s.persistNewContract(ctx, actor, c, external, filter)
}

func (s *Service) prepareContract(ctx context.Context, actor Principal, c contract.Contract, external bool, permission string) (contract.Contract, error) {
	filter, ok := actor.Scope(permission)
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
	if !external && c.TemplateID != "" {
		rendered, normalizedValues, err := s.renderTemplateForPermission(ctx, actor, permission, c.TemplateID, c.TemplateValues, c.AmountMinor)
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
	c.Title = strings.TrimSpace(c.Title)
	c.Type = strings.TrimSpace(c.Type)
	c.OpportunityID = strings.TrimSpace(c.OpportunityID)
	c.OpportunityName = strings.TrimSpace(c.OpportunityName)
	c.CustomerName = strings.TrimSpace(c.CustomerName)
	c.CustomerAddress = strings.TrimSpace(c.CustomerAddress)
	c.CustomerContact = strings.TrimSpace(c.CustomerContact)
	c.CustomerPhone = strings.TrimSpace(c.CustomerPhone)
	if external {
		c.ServiceItems = normalizeExternalServiceItems(c.ServiceItems)
	}
	if err := validateContractTextLengths(c); err != nil {
		return c, err
	}
	commonInvalid := c.Title == "" || c.Type == "" || c.AmountMinor < 0 || c.Content == "" && len(c.Document) == 0
	if external {
		if commonInvalid || c.TemplateID != "" || c.Type != "直签" && c.Type != "三方" || c.CRMCustomerID == 0 || c.CustomerName == "" || c.AmountMinor <= 0 || !validExternalServiceItems(c.ServiceItems) || c.OpportunityID != "" && c.OpportunityName == "" || c.OpportunityID == "" && c.OpportunityName != "" || !validOptionalCRMID(c.OpportunityID) {
			return c, ErrValidation
		}
		reference, err := s.validateExternalCRMReference(ctx, c.CRMCustomerID, c.OpportunityID, actor.IdentityID)
		if err != nil {
			return c, err
		}
		c.CustomerName = strings.TrimSpace(reference.Customer.Name)
		if reference.Opportunity != nil {
			c.OpportunityName = strings.TrimSpace(reference.Opportunity.Name)
		}
		categories := make([]string, 0, len(c.ServiceItems))
		for _, item := range c.ServiceItems {
			categories = append(categories, item.Category)
		}
		if err := s.validateExternalDetectionCategories(ctx, categories); err != nil {
			return c, err
		}
	} else if commonInvalid || c.TemplateID == "" || !validServiceItems(c.ServiceItems) || c.OpportunityID != "" && c.CRMCustomerID == 0 {
		return c, ErrValidation
	}
	// 模板合同的 CRM 引用与付款计划是否存在解耦：只要出现非空客户/商机引用，
	// 就必须经过权威目录校验，避免“模板+客户+商机+无付款计划”绕过客户有效性、
	// 商机归属和用户数据范围检查。付款计划本身也要求有效客户。
	if !external && (c.CRMCustomerID > 0 || c.OpportunityID != "" || len(c.PaymentTerms) > 0) {
		if c.CRMCustomerID == 0 || !validOptionalCRMID(c.OpportunityID) {
			return c, fmt.Errorf("%w: 付款计划或商机引用须关联有效的CRM客户", ErrValidation)
		}
		reference, err := s.validateExternalCRMReference(ctx, c.CRMCustomerID, c.OpportunityID, actor.IdentityID)
		if err != nil {
			return c, err
		}
		c.CustomerName = strings.TrimSpace(reference.Customer.Name)
		if reference.Opportunity != nil {
			c.OpportunityName = strings.TrimSpace(reference.Opportunity.Name)
		} else {
			c.OpportunityName = ""
		}
	}
	c.ServiceType = c.ServiceItems[0].ServiceType
	c.Systems = flattenedSystems(c.ServiceItems)
	if c.StartDate != nil && c.EndDate != nil && c.StartDate.After(*c.EndDate) {
		return c, ErrValidation
	}
	if c.Currency == "" {
		c.Currency = "CNY"
	}
	c.Currency = strings.ToUpper(strings.TrimSpace(c.Currency))
	if len(c.Currency) != 3 {
		return c, fmt.Errorf("%w: 币种必须为三位字母代码", ErrValidation)
	}
	for _, char := range c.Currency {
		if char < 'A' || char > 'Z' {
			return c, fmt.Errorf("%w: 币种必须为三位字母代码", ErrValidation)
		}
	}
	terms, err := contract.NormalizePaymentTerms(c.PaymentTerms, c.AmountMinor, c.Currency)
	if err != nil {
		return c, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	c.PaymentTerms = terms
	if c.NumberFormat == "" {
		c.NumberFormat = contracttemplate.DefaultNumberFormat
	}
	return c, nil
}

// validateContractTextLengths 与前端 maxlength/DB 列宽保持一致，避免绕过页面后
// 由 MySQL 截断或抛出 500；所有长度按字符数（rune）计算。
func validateContractTextLengths(c contract.Contract) error {
	checks := []struct {
		value string
		label string
		max   int
	}{
		{c.Title, "合同名称", 255}, {c.Type, "合同类型", 64},
		{c.OpportunityID, "商机 ID", 64}, {c.OpportunityName, "商机名称", 255},
		{c.CustomerName, "客户名称", 255}, {c.CustomerAddress, "客户地址", 500},
		{c.CustomerContact, "客户联系人", 128}, {c.CustomerPhone, "客户联系电话", 64},
		{c.CustomerCreditLevel, "客户信用等级", 16}, {c.NumberFormat, "合同编号格式", 160},
		{c.TemplateID, "模板 ID", 26}, {c.ProjectID, "项目 ID", 64}, {c.OwnerOrgID, "组织 ID", 64},
	}
	for _, check := range checks {
		if len([]rune(strings.TrimSpace(check.value))) > check.max {
			return fmt.Errorf("%w: %s不能超过 %d 个字符", ErrValidation, check.label, check.max)
		}
	}
	for index, item := range c.ServiceItems {
		itemChecks := []struct {
			value string
			label string
			max   int
		}{
			{item.ServiceType, "服务类型", 64}, {item.Name, "服务名称", 255},
			{item.Site, "实施场所", 255}, {item.Batch, "实施批次", 120},
			{item.Category, "检测类别", 120}, {item.Requirement, "服务需求", 1000},
		}
		for _, check := range itemChecks {
			if len([]rune(strings.TrimSpace(check.value))) > check.max {
				return fmt.Errorf("%w: 服务项 %d 的%s不能超过 %d 个字符", ErrValidation, index+1, check.label, check.max)
			}
		}
		for _, system := range item.Systems {
			if len([]rune(strings.TrimSpace(system.Name))) > 255 {
				return fmt.Errorf("%w: 服务项 %d 的系统名称不能超过 255 个字符", ErrValidation, index+1)
			}
		}
	}
	for name, value := range c.TemplateValues {
		if len([]rune(value)) > 2000 {
			return fmt.Errorf("%w: 模板字段“%s”不能超过 2000 个字符", ErrValidation, name)
		}
	}
	return nil
}

func (s *Service) persistNewContract(ctx context.Context, actor Principal, c contract.Contract, external bool, filter contract.ScopeFilter) (contract.Contract, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.Contract{}, err
	}
	c.ID, c.TenantID, c.OwnerUserID, c.OwnerDisplayName, c.Status = ulid.Make().String(), actor.TenantID, actor.UserID, actor.DisplayName, contract.StatusDraft
	c.CreatedBy = actor.UserID
	c.Number = "" // 草稿绝不接受调用方提供的正式合同编号。
	if c.OwnerIdentityID == "" && filter.AllowAll {
		c.OwnerIdentityID = actor.IdentityID
	}
	hashSource := []byte(c.Content)
	if len(c.Document) > 0 {
		hashSource = c.Document
	}
	hash := sha256.Sum256(hashSource)
	c.ContentHash = hex.EncodeToString(hash[:])
	if external && s.StampedFileGateway != nil && !strings.EqualFold(strings.TrimSpace(s.StampedFileMode), "legacy") {
		fileID, uploadErr := s.StampedFileGateway.Upload(ctx, "contract-external-"+c.ID, s.StampedFileApplicationID, "CONTRACT_EXTERNAL_SOURCE", "external-contract.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", bytes.NewReader(c.Document))
		if uploadErr != nil {
			if strings.EqualFold(strings.TrimSpace(s.StampedFileMode), "required") {
				return c, fmt.Errorf("contract source file gateway: %w", uploadErr)
			}
			c.SourceFileStatus = "FAILED"
		} else {
			c.SourceFileID, c.SourceFileStatus = fileID, "PENDING"
		}
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return contract.Contract{}, err
	}
	if err := s.Repo.CreateContract(ctx, c, actor.UserID); err != nil {
		return c, err
	}
	if external && c.SourceFileID != "" {
		bindErr := s.StampedFileGateway.Bind(ctx, s.StampedFileApplicationID, c.SourceFileID, "contract", c.ID, "EXTERNAL_SOURCE", c.Title+".docx")
		state, detail := "READY", ""
		if bindErr != nil {
			state, detail = "FAILED", bindErr.Error()
		}
		c.SourceFileStatus = state
		if repository, ok := s.Repo.(SourceFileGatewayStateRepository); ok {
			_ = repository.MarkContractSourceGatewayResult(ctx, c.TenantID, c.ID, c.SourceFileID, state, detail)
		}
	}
	c.Version = 1
	return c, nil
}

func validOptionalCRMID(value string) bool {
	if value == "" {
		return true
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0
}

func normalizeExternalServiceItems(items []contract.ServiceItem) []contract.ServiceItem {
	normalized := append([]contract.ServiceItem(nil), items...)
	for index := range normalized {
		item := &normalized[index]
		item.SourceID = ""
		item.ServiceType = strings.TrimSpace(item.ServiceType)
		item.Name = strings.TrimSpace(item.Name)
		if item.Name == "" {
			item.Name = item.ServiceType
		}
		item.Site = strings.TrimSpace(item.Site)
		item.Batch = strings.TrimSpace(item.Batch)
		item.Category = strings.TrimSpace(item.Category)
		item.Requirement = strings.TrimSpace(item.Requirement)
		item.TestMode = strings.ToUpper(strings.TrimSpace(item.TestMode))
		item.Systems = append([]contract.SystemInfo(nil), item.Systems...)
		for systemIndex := range item.Systems {
			item.Systems[systemIndex].Name = strings.TrimSpace(item.Systems[systemIndex].Name)
			item.Systems[systemIndex].Level = strings.TrimSpace(item.Systems[systemIndex].Level)
		}
	}
	return normalized
}

func validExternalServiceItems(items []contract.ServiceItem) bool {
	if len(items) == 0 || len(items) > 20 {
		return false
	}
	for _, item := range items {
		if item.ServiceType == "" || item.Site == "" || item.Batch == "" || item.Category == "" ||
			(item.TestMode != "STANDARD" && item.TestMode != "PENETRATION") || !validSystems(item.Systems) {
			return false
		}
	}
	return true
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
	return s.SubmitContractVersion(ctx, actor, contractID, termsIdentical, 0)
}

func (s *Service) SubmitContractVersion(ctx context.Context, actor Principal, contractID string, termsIdentical bool, expectedVersion uint64) (StartResult, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return StartResult{}, err
	}
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
	if expectedVersion != 0 && c.Version != expectedVersion {
		return StartResult{}, apperrors.ErrVersionConflict
	}
	if !draftActorAllowed(actor, c) {
		return StartResult{}, ErrForbidden
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
	in := workflows.ContractApprovalInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: contractID, ContractVersion: c.Version, ApplicantUserID: actor.UserID, ApplicantDisplayName: actor.DisplayName, ContentHash: c.ContentHash, RuleID: ruleID, RuleVersion: ruleVersion, Nodes: nodes, DefaultNodeTimeout: s.NodeTimeout, ReminderInterval: s.ReminderInterval, Audit: auditMetaFromContext(ctx)}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return StartResult{}, err
	}
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
	if err := s.checkBusinessLicense(ctx); err != nil {
		return StartResult{}, err
	}
	filter, ok := s.contractScope(actor, "contract.edit")
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
		if err := s.checkBusinessLicense(ctx); err != nil {
			return StartResult{}, err
		}
		return StartResult{}, s.Repo.TransitionDirect(ctx, actor.TenantID, contractID, version, target, actor.UserID, reason, key)
	}
	directory, directoryErr := s.loadPersonnel(ctx, actor, []string{"admin"})
	if directoryErr != nil {
		return StartResult{}, directoryErr
	}
	admins := approversForRole(directory, "admin")
	if len(admins) == 0 {
		return StartResult{}, &ApprovalAssigneeUnavailableError{RoleCodes: []string{"admin"}}
	}
	approvalID := ulid.Make().String()
	workflowID := fmt.Sprintf("status-change:%s:%s:v%d", actor.TenantID, contractID, version)
	in := workflows.StatusChangeInput{ApprovalID: approvalID, TenantID: actor.TenantID, ContractID: contractID, ContractVersion: version, ApplicantUserID: actor.UserID, ApplicantDisplayName: actor.DisplayName, FromStatus: c.Status, TargetStatus: target, Reason: reason, AdminUserIDs: admins, Timeout: s.NodeTimeout, Audit: auditMetaFromContext(ctx)}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return StartResult{}, err
	}
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
	if err := s.checkBusinessLicense(ctx); err != nil {
		return "", err
	}
	meta, state, err := s.queryApprovalState(ctx, actor, approvalID)
	if err != nil {
		return "", err
	}
	if meta.Status != approval.StatusRunning {
		return "", apperrors.ErrStateConflict
	}
	// 转派命令必须绑定当前节点 ID；前端可以不传，但不能让工作流转派落到错误节点。
	if command.Action == workflows.ActionTransfer && command.TargetNodeID == "" &&
		state.CurrentNodeIndex >= 0 && state.CurrentNodeIndex < len(state.Nodes) {
		command.TargetNodeID = state.Nodes[state.CurrentNodeIndex].Node.ID
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
	if len([]rune(command.Comment)) > 2000 {
		return "", fmt.Errorf("%w: 审批意见不能超过 2000 个字符", ErrValidation)
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
	// 审批规则节点指派总监角色的所有有效用户。新命令携带
	// 或签策略，使运行中的工作流无需在引入该行为时
	// 改写历史决策。
	command.RoleNodeOrSign = command.Action == workflows.ActionApprove
	command.CommandID, command.ActorUserID, command.ActorDisplayName, command.OccurredAt = ulid.Make().String(), actor.UserID, actor.DisplayName, time.Now().UTC()
	if err := s.checkBusinessLicense(ctx); err != nil {
		return "", err
	}
	if err := s.Temporal.SignalWorkflow(ctx, meta.WorkflowID, meta.RunID, workflows.CommandSignalName, command); err != nil {
		return "", fmt.Errorf("%w: signal approval workflow %s: %v", ErrApprovalWorkflowUnavailable, meta.WorkflowID, err)
	}
	return command.CommandID, nil
}

// ReassignApprovalNode 通过权威工作流命令把审批节点从 fromUserID 转派给 toUserID。
// Signal 被接收不等于已经执行：这里会查询工作流状态，确认当前节点实际完成转派后才
// 返回证据；幂等键生成稳定命令 ID，重试不会二次转派。
func (s *Service) ReassignApprovalNode(ctx context.Context, tenantID, approvalID, nodeID, fromUserID, toUserID, reason, idempotencyKey string) (string, error) {
	tenantID, approvalID, nodeID = strings.TrimSpace(tenantID), strings.TrimSpace(approvalID), strings.TrimSpace(nodeID)
	fromUserID, toUserID = strings.TrimSpace(fromUserID), strings.TrimSpace(toUserID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if tenantID == "" || approvalID == "" || nodeID == "" || fromUserID == "" || toUserID == "" || fromUserID == toUserID || idempotencyKey == "" {
		return "", ErrValidation
	}
	if s.Temporal == nil {
		return "", fmt.Errorf("approval workflow client is unavailable: %w", ErrHandoverConflict)
	}
	meta, err := s.Repo.GetApprovalMeta(ctx, tenantID, approvalID)
	if err != nil {
		return "", err
	}
	if meta.Status != approval.StatusRunning {
		return "", fmt.Errorf("approval %s is %s and cannot transfer: %w", approvalID, meta.Status, ErrHandoverConflict)
	}
	state, err := s.queryApprovalWorkflowState(ctx, meta.WorkflowID, meta.RunID)
	if err != nil {
		return "", err
	}
	evidence := "approval-workflow:" + meta.WorkflowID + "/" + meta.RunID + "#" + handoverCommandID(idempotencyKey)
	node := runtimeNodeByID(state, nodeID)
	if node == nil {
		return "", fmt.Errorf("approval node %s not found: %w", nodeID, ErrHandoverConflict)
	}
	if containsString(node.Node.AssigneeIDs, toUserID) && !containsString(node.Node.AssigneeIDs, fromUserID) {
		// 上一次已执行、但回执未落库：同一幂等键直接视为完成。
		return evidence, nil
	}
	if node.Status != approval.NodeActive || !containsString(node.Node.AssigneeIDs, fromUserID) {
		return "", fmt.Errorf("approval node %s is not active for %s: %w", nodeID, fromUserID, ErrHandoverConflict)
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return "", err
	}
	command := workflows.ApprovalCommand{
		CommandID: handoverCommandID(idempotencyKey), Action: workflows.ActionTransfer,
		ActorUserID: fromUserID, Comment: reason, TargetUserIDs: []string{toUserID},
		TargetNodeID: nodeID, OccurredAt: time.Now().UTC(),
	}
	if err := s.Temporal.SignalWorkflow(ctx, meta.WorkflowID, meta.RunID, workflows.CommandSignalName, command); err != nil {
		return "", fmt.Errorf("signal approval transfer %s: %w", meta.WorkflowID, err)
	}
	// 等待命令被工作流实际消费；超时返回可重试错误，交接服务据此写 RETRYABLE，
	// 绝不在 Signal 刚返回时就宣告 APPLIED。
	deadline := time.Now().Add(8 * time.Second)
	for {
		current, queryErr := s.queryApprovalWorkflowState(ctx, meta.WorkflowID, meta.RunID)
		if queryErr == nil {
			applied := runtimeNodeByID(current, nodeID)
			if applied != nil && containsString(applied.Node.AssigneeIDs, toUserID) && !containsString(applied.Node.AssigneeIDs, fromUserID) {
				return evidence, nil
			}
			if current.Status != approval.StatusRunning {
				return "", fmt.Errorf("approval %s completed before transfer was applied: %w", approvalID, ErrHandoverConflict)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("approval transfer signal was accepted but not applied in time: %w", ErrHandoverConflict)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func auditMetaFromContext(ctx context.Context) workflows.AuditMeta {
	meta := auditctx.FromContext(ctx)
	return workflows.AuditMeta{LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID}
}

func (s *Service) queryApprovalWorkflowState(ctx context.Context, workflowID, runID string) (workflows.ApprovalState, error) {
	encoded, err := s.Temporal.QueryWorkflow(ctx, workflowID, runID, workflows.StateQueryName)
	if err != nil {
		return workflows.ApprovalState{}, fmt.Errorf("%w: query approval workflow %s: %v", ErrApprovalWorkflowUnavailable, workflowID, err)
	}
	var state workflows.ApprovalState
	if err := encoded.Get(&state); err != nil {
		return workflows.ApprovalState{}, fmt.Errorf("%w: decode approval workflow %s state: %v", ErrApprovalWorkflowUnavailable, workflowID, err)
	}
	return state, nil
}

func runtimeNodeByID(state workflows.ApprovalState, nodeID string) *workflows.RuntimeNode {
	for index := range state.Nodes {
		if state.Nodes[index].Node.ID == nodeID {
			return &state.Nodes[index]
		}
	}
	return nil
}

// handoverCommandID 由交接幂等键派生固定 26 位命令 ID（与 con_approval_action.command_id 对齐），
// 保证 Signal 重试复用同一个工作流命令。
func handoverCommandID(idempotencyKey string) string {
	digest := sha256.Sum256([]byte("handover-command:" + idempotencyKey))
	return hex.EncodeToString(digest[:13])
}

func (s *Service) GetApprovalDetail(ctx context.Context, actor Principal, approvalID string) (ApprovalDetail, error) {
	meta, state, err := s.queryApprovalState(ctx, actor, approvalID)
	if err != nil && !errors.Is(err, ErrApprovalWorkflowUnavailable) {
		return ApprovalDetail{}, err
	}
	stateUnavailable := err != nil
	if stateUnavailable {
		// 工作流查询可能过期，但持久化审批快照
		// 仍是权威历史提交。返回快照前，
		// 按同一合同边界重新授权。
		meta, err = s.Repo.GetApprovalMeta(ctx, actor.TenantID, approvalID)
		if err != nil {
			return ApprovalDetail{}, err
		}
		if _, err = s.getApprovalContract(ctx, actor, meta.ContractID); err != nil {
			return ApprovalDetail{}, err
		}
		if !actor.Has("approval.view") && !actor.Has("approval.process") && meta.ApplicantUserID != actor.UserID {
			return ApprovalDetail{}, ErrForbidden
		}
	}
	// 先用当前合同完成数据范围校验（权限仍按合同边界执行），再展示审批发起时的
	// 不可变快照；旧审批没有快照时才降级到当前合同。
	contractSnapshot, err := s.getApprovalContract(ctx, actor, meta.ContractID)
	if err != nil {
		return ApprovalDetail{}, err
	}
	snapshotAvailable := false
	if snapshot, found, snapshotErr := s.Repo.GetApprovalSnapshot(ctx, actor.TenantID, approvalID); snapshotErr != nil {
		return ApprovalDetail{}, snapshotErr
	} else if found {
		contractSnapshot, snapshotAvailable = snapshot, true
	}
	if stateUnavailable && !snapshotAvailable {
		return ApprovalDetail{}, ErrApprovalWorkflowUnavailable
	}
	actions, err := s.Repo.ListApprovalActions(ctx, actor.TenantID, approvalID)
	if err != nil {
		return ApprovalDetail{}, err
	}
	if actions == nil {
		actions = []approval.Action{}
	}
	return ApprovalDetail{Meta: meta, State: state, Contract: contractSnapshot, Actions: actions, SnapshotAvailable: snapshotAvailable, StateUnavailable: stateUnavailable}, nil
}

// RecoverApproval 是工作流执行故障后的受控收口入口：只有工作流已停止、且持久化
// 运行状态已到达终态时才调用幂等的 CompleteApproval，不猜测运行中的节点结果。
func (s *Service) RecoverApproval(ctx context.Context, actor Principal, approvalID string) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if !actor.Has("approval.manage") && !hasRole(actor, "admin") {
		return ErrForbidden
	}
	meta, err := s.Repo.GetApprovalMeta(ctx, actor.TenantID, strings.TrimSpace(approvalID))
	if err != nil {
		return err
	}
	if meta.Status != approval.StatusRunning {
		return fmt.Errorf("%w: 审批已结束，无需恢复", apperrors.ErrStateConflict)
	}
	store, ok := s.Repo.(ApprovalRecoveryStore)
	if !ok {
		return fmt.Errorf("approval recovery store is not configured")
	}
	if s.Temporal == nil {
		return fmt.Errorf("approval workflow client is unavailable: %w", ErrApprovalWorkflowUnavailable)
	}
	desc, err := s.Temporal.DescribeWorkflowExecution(ctx, meta.WorkflowID, meta.RunID)
	if err != nil {
		return fmt.Errorf("%w: describe approval workflow %s: %v", ErrApprovalWorkflowUnavailable, meta.WorkflowID, err)
	}
	if desc.GetWorkflowExecutionInfo().GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return fmt.Errorf("%w: 工作流仍在运行，不能人工收口；请等待执行或检查 Worker", apperrors.ErrStateConflict)
	}
	state, found, err := store.GetApprovalRuntimeState(ctx, actor.TenantID, meta.ID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: 审批没有持久化运行状态，无法安全恢复", apperrors.ErrStateConflict)
	}
	status := state.Status
	if status == approval.StatusRunning {
		// 最后一个节点已通过时，工作流会在 CompleteApproval 之前把索引推进到末尾；
		// 只有这种明确的终态才允许自动判定通过，运行中节点必须人工复核。
		if state.CurrentNodeIndex < 0 || state.CurrentNodeIndex < len(state.Nodes) {
			return fmt.Errorf("%w: 工作流停在未完成的审批节点，需人工复核后再处理", apperrors.ErrStateConflict)
		}
		status = approval.StatusApproved
	}
	switch status {
	case approval.StatusApproved, approval.StatusRejected, approval.StatusWithdrawn, approval.StatusExpired:
	default:
		return fmt.Errorf("%w: 审批运行状态 %s 不能直接收口", apperrors.ErrStateConflict, status)
	}
	target := contract.StatusDraft
	if meta.Kind == approval.KindStatusChange {
		target = contract.Status(meta.FromStatus)
		if status == approval.StatusApproved {
			target = contract.Status(meta.TargetStatus)
		}
	} else if status == approval.StatusApproved {
		target = contract.StatusActive
	}
	input := workflows.CompleteApprovalActivityInput{
		ApprovalID: meta.ID, TenantID: meta.TenantID, ContractID: meta.ContractID,
		ActorUserID: actor.UserID, Status: status, Reason: "manual recovery after workflow execution failure",
		WorkflowID: meta.WorkflowID, TargetStatus: target,
	}
	return store.CompleteApproval(ctx, input)
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
	item, err := s.getContractScoped(ctx, filter, id)
	item.CanEditDraft = s.CanEditDraft(actor, item)
	return item, err
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
		items, err := scoped.ListContractsScoped(ctx, filter, keyword, status, limit)
		for i := range items {
			items[i].CanEditDraft = s.CanEditDraft(actor, items[i])
		}
		return items, err
	}
	if !filter.AllowAll && !filter.AllowSelf && len(filter.OrganizationIDs) == 0 && len(filter.ProjectIDs) == 0 {
		return nil, ErrForbidden
	}
	ownerUserID := ""
	if filter.AllowSelf {
		ownerUserID = actor.UserID
	}
	items, err := s.Repo.ListContracts(ctx, actor.TenantID, keyword, ownerUserID, status, limit)
	for i := range items {
		items[i].CanEditDraft = s.CanEditDraft(actor, items[i])
	}
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
	if err := s.checkBusinessLicense(ctx); err != nil {
		return approval.Rule{}, err
	}
	if !actor.Has("approval_rule.manage") {
		return rule, ErrForbidden
	}
	if err := validateRule(rule); err != nil {
		return rule, err
	}
	rule.ID, rule.TenantID, rule.Version = ulid.Make().String(), actor.TenantID, 1
	if err := s.checkBusinessLicense(ctx); err != nil {
		return approval.Rule{}, err
	}
	if err := s.Repo.CreateRule(ctx, rule, actor.UserID); err != nil {
		return rule, err
	}
	return rule, nil
}

func (s *Service) UpdateRule(ctx context.Context, actor Principal, rule approval.Rule) (approval.Rule, error) {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return approval.Rule{}, err
	}
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
	if err := s.checkBusinessLicense(ctx); err != nil {
		return approval.Rule{}, err
	}
	if err := s.Repo.UpdateRule(ctx, rule, actor.UserID); err != nil {
		return rule, err
	}
	rule.Version++
	return rule, nil
}

func (s *Service) DeleteRule(ctx context.Context, actor Principal, id string, version uint64) error {
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	if !actor.Has("approval_rule.manage") {
		return ErrForbidden
	}
	if id == "" || version == 0 {
		return ErrValidation
	}
	if err := s.checkBusinessLicense(ctx); err != nil {
		return err
	}
	return s.Repo.DeleteRule(ctx, actor.TenantID, id, version)
}

func validateRule(rule approval.Rule) error {
	if rule.Name == "" || len(rule.Nodes) == 0 {
		return ErrValidation
	}
	if len([]rune(rule.Name)) > 128 {
		return fmt.Errorf("%w: 审批规则名称不能超过 128 个字符", ErrValidation)
	}
	if _, err := rule.Expression.Match(approval.Facts{}); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	seen := map[string]bool{}
	for _, node := range rule.Nodes {
		if node.ID == "" || node.Name == "" || node.RoleCode == "" || seen[node.ID] {
			return ErrValidation
		}
		if len([]rune(node.ID)) > 64 || len([]rune(node.Name)) > 128 || len([]rune(node.RoleCode)) > 128 {
			return fmt.Errorf("%w: 审批节点 ID/名称/角色编码超出长度限制", ErrValidation)
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
	unresolved := make([]string, 0, len(nodes))
	for i := range nodes {
		// 受理人始终从平台当前有效角色重建。有意忽略持久化的
		// 规则受理人 ID，避免人员变化后需要
		// 编辑审批规则。
		nodes[i].AssigneeIDs = approversForRole(directory, nodes[i].RoleCode)
		nodes[i].AssigneeIDs = unique(nodes[i].AssigneeIDs)
		nodes[i].Countersign = approval.CountersignAny
		if nodes[i].ID == "" {
			nodes[i].ID = fmt.Sprintf("node-%d", i+1)
		}
		if len(nodes[i].AssigneeIDs) == 0 {
			unresolved = append(unresolved, nodes[i].RoleCode)
		}
	}
	if len(unresolved) > 0 {
		return &ApprovalAssigneeUnavailableError{RoleCodes: unique(unresolved)}
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
	// 合同领域纵深防御：普通销售用户始终只能访问
	// 当前身份拥有的合同，即使过期的平台声明
	// 意外携带 APPLICATION/TENANT 全范围。具有更高合同权限的
	// 角色仍按明确签发的范围执行审批。
	if (permission == "contract.read" || permission == "contract.edit") && salesOnly(actor.Roles) {
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
	ErrForbidden                             = errors.New("forbidden")
	ErrValidation                            = errors.New("validation failed")
	ErrApprovalTargetForbidden               = errors.New("approval target cannot process contract approvals")
	ErrApprovalAssigneeUnavailable           = errors.New("approval assignee is unavailable")
	ErrPersonnelDirectoryUnavailable         = errors.New("approval personnel directory is unavailable")
	ErrApprovalWorkflowUnavailable           = errors.New("approval workflow is unavailable")
	ErrDetectionCategoryDirectoryUnavailable = errors.New("detection category directory is unavailable")
)

// ApprovalAssigneeUnavailableError 表示审批节点配置的角色在基础平台没有有效成员。
// 这发生在审批实例和合同状态写入之前，是确定性的配置缺失而非结果未知的写入失败；
// HTTP 层必须据此给出“本次未提交”的明确指引，不能按 500 处理并诱发重复提交。
type ApprovalAssigneeUnavailableError struct {
	RoleCodes []string
}

func (e *ApprovalAssigneeUnavailableError) Error() string {
	return "no active platform user has role " + strings.Join(e.RoleCodes, ",")
}

func (e *ApprovalAssigneeUnavailableError) Unwrap() error { return ErrApprovalAssigneeUnavailable }
