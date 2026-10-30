package application

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"slices"
	"strconv"
	"time"
)

// importHistoryOwner 解析目标责任人并复核委派范围和交接冻结。
// 参数：ctx为上下文，actor为可信操作者，item为冻结清单。
// 返回值：当前目录负责人或权限、目录、冻结错误。
func (s *Service) importHistoryOwner(ctx context.Context, actor Principal, item contract.ImportItem) (UserReference, error) {
	if item.HistoryEvidence == nil || s.ImportOwners == nil {
		return UserReference{}, ErrImportUnsupported
	}
	id := item.HistoryEvidence.TargetOwnerUserID
	scope, ok := actor.Scope("contract.import.commit")
	if !ok || (!scope.AllowAll && id != actor.UserID) {
		return UserReference{}, ErrForbidden
	}
	owner, e := s.ImportOwners.ResolveOwner(ctx, actor.TenantID, id)
	if e != nil {
		return owner, e
	}
	if owner.UserID != id {
		return owner, ErrForbidden
	}
	return owner, s.EnsureOwnerNotFrozen(ctx, actor.TenantID, id)
}

// importConfirmationAccess 仅授予受派且当前仍具审批权限的身份读取确认。
// 参数：actor为可信当前身份，b为目标台账。
// 返回值：无有效受派角色及范围时返回禁止。
func importConfirmationAccess(actor Principal, b contract.ImportBatch) error {
	if b.TenantID != actor.TenantID || b.Confirmation == nil {
		return ErrForbidden
	}
	if _, ok := actor.Scope("contract.import.approve"); !ok {
		return ErrForbidden
	}
	for role, users := range b.Confirmation.Assignees {
		if slices.Contains(users, actor.UserID) && slices.Contains(actor.Roles, role) {
			return nil
		}
	}
	return ErrForbidden
}

// SubmitImportConfirmation 冻结独立历史审批与异步原件建单身份。
// 参数：ctx及actor为当前会话，id/revision/token为预检基线，key为稳定命令键，session为受信会话哈希。
// 返回值：已受理台账或审批角色、版本、权限及Temporal启动错误。
func (s *Service) SubmitImportConfirmation(ctx context.Context, actor Principal, id string, revision uint64, token, key string, session []byte) (contract.ImportBatch, error) {
	r, e := s.importStore()
	if e != nil {
		return contract.ImportBatch{}, e
	}
	b, e := s.GetImportBatch(ctx, actor, id)
	if e != nil {
		return b, e
	}
	if e = importAccess(actor, b, "contract.import.commit"); e != nil {
		return b, e
	}
	if actor.UserID != b.ActorID || len(session) != 32 || len(key) < 8 || len(key) > 128 {
		return b, ErrValidation
	}
	keyHash, _ := contract.ImportDigest(key)
	if b.Confirmation != nil {
		if b.Revision != revision || b.CommitKeyHash != keyHash {
			return b, ErrImportConflict
		}
		return b, s.startImportConfirmation(ctx, b)
	}
	if !s.ContractImportHistoryEnabled || !s.ContractImportEnabled || s.Temporal == nil {
		return b, ErrImportUnsupported
	}
	if b.Revision != revision || b.State != "PREVIEWED" || b.PreviewToken != token || token == "" || !b.PreviewExpiresAt.After(time.Now()) {
		return b, ErrImportConflict
	}
	if e = s.checkBusinessLicense(ctx); e != nil {
		return b, e
	}
	roles := s.ContractImportConfirmationRoles
	if len(roles) == 0 {
		return b, &ApprovalAssigneeUnavailableError{RoleCodes: []string{"CONTRACT_IMPORT_CONFIRMATION_ROLES"}}
	}
	users, e := s.loadPersonnel(ctx, actor, roles)
	if e != nil {
		return b, e
	}
	assigned := map[string][]string{}
	for _, role := range roles {
		assigned[role] = approversForRole(users, role)
		if len(assigned[role]) == 0 {
			return b, &ApprovalAssigneeUnavailableError{RoleCodes: []string{role}}
		}
	}
	for _, item := range b.Items {
		if item.Item.Mode == contract.ImportNormal || contract.ValidateHistoryEvidence(item.Item) != nil || len(importFieldIssues(item.Item)) > 0 || item.Digest == "" {
			return b, ErrValidation
		}
		if _, e = s.importHistoryOwner(ctx, actor, item.Item); e != nil {
			return b, e
		}
	}
	digest, _ := contract.ImportDigest(struct {
		Revision uint64
		Items    []contract.ImportItemState
		Sources  []contract.ImportSource
	}{b.Revision, b.Items, b.Sources})
	now := time.Now().UTC()
	expected := b.Version
	e = r.UpdateImportBatch(ctx, b.TenantID, b.ID, func(current *contract.ImportBatch) error {
		if current.Version != expected || current.Revision != revision || current.Confirmation != nil {
			return ErrImportConflict
		}
		current.Confirmation = &contract.ImportConfirmation{WorkflowID: "contract-import-confirmation-" + b.ID + "-" + strconv.FormatUint(revision, 10),
			Revision:      revision,
			Digest:        digest,
			State:         "STARTING",
			Roles:         roles,
			Assignees:     assigned,
			ApprovedRoles: []string{},
			Commands:      []contract.ImportConfirmationCommand{},
			SubmittedAt:   now,
			ExpiresAt:     now.Add(72 * time.Hour)}
		current.State, current.OperationID, current.CommittedRevision = "QUEUED", "history-"+b.ID, revision
		current.SessionHash, current.CommitKeyHash = append([]byte(nil), session...), keyHash
		current.Epoch++
		return nil
	})
	if e != nil {
		return b, e
	}
	b, e = r.GetImportBatch(ctx, actor.TenantID, id)
	if e != nil {
		return b, e
	}
	e = s.startImportConfirmation(ctx, b)
	return b, e
}

// startImportConfirmation 使用稳定WorkflowID恢复未知启动响应。
// 参数：ctx为上下文，b为持久化冻结台账。
// 返回值：启动成功、已存在或明确Temporal不可用错误。
func (s *Service) startImportConfirmation(ctx context.Context, b contract.ImportBatch) error {
	if s.Temporal == nil {
		return ErrApprovalWorkflowUnavailable
	}
	_, e := s.Temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: b.Confirmation.WorkflowID, TaskQueue: s.taskQueue(), WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, workflows.ImportConfirmationWorkflowName, workflows.ImportConfirmationInput{TenantID: b.TenantID, BatchID: b.ID, Revision: b.Revision, Digest: b.Confirmation.Digest})
	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(e, &already) {
		return nil
	}
	return e
}

// ActImportConfirmation 持久化可信当前身份的审批命令供独立Workflow执行。
// 参数：ctx及actor为当前鉴权，id/version为批次围栏，key/action为稳定批准、驳回或撤回命令。
// 返回值：权限、冲突或存储错误；不接受客户端审批结果字段。
func (s *Service) ActImportConfirmation(ctx context.Context, actor Principal, id string, version uint64, key, action string) error {
	if version == 0 || len(key) < 8 || len(key) > 128 || (action != "APPROVE" && action != "REJECT" && action != "WITHDRAW") {
		return ErrValidation
	}
	if e := s.checkBusinessLicense(ctx); e != nil {
		return e
	}
	r, e := s.importStore()
	if e != nil {
		return e
	}
	return r.UpdateImportBatch(ctx, actor.TenantID, id, func(b *contract.ImportBatch) error {
		if b.Confirmation == nil {
			return ErrImportConflict
		}
		cf := b.Confirmation
		if action == "WITHDRAW" {
			if actor.UserID != b.ActorID {
				return ErrForbidden
			}
			if e := importAccess(actor, *b, "contract.import.commit"); e != nil {
				return e
			}
		} else if e := importConfirmationAccess(actor, *b); e != nil {
			return e
		}
		for _, cmd := range cf.Commands {
			if cmd.Key == key {
				if cmd.ActorID != actor.UserID || cmd.Action != action {
					return ErrImportConflict
				}
				return nil
			}
		}
		if b.Version != version || (cf.State != "PENDING" && !(action == "WITHDRAW" && cf.State == "STARTING")) || !cf.ExpiresAt.After(time.Now()) {
			return ErrImportConflict
		}
		if len(cf.Commands) >= 1000 {
			return ErrValidation
		}
		roles := []string{}
		for role, users := range cf.Assignees {
			if slices.Contains(users, actor.UserID) && slices.Contains(actor.Roles, role) {
				roles = append(roles, role)
			}
		}
		cf.Commands = append(cf.Commands, contract.ImportConfirmationCommand{Key: key, ActorID: actor.UserID, Action: action, Roles: roles, At: time.Now().UTC()})
		return nil
	})
}

// importHistoryModeAvailable 确认新增历史模式的真实机器能力已配置。
// 参数：mode为新清单历史模式。
// 返回值：仅所有必要出站与报告入站能力已装配时为true。
func (s *Service) importHistoryModeAvailable(mode contract.ImportMode) bool {
	if !s.ContractImportHistoryEnabled || len(s.ContractImportConfirmationRoles) == 0 || s.ImportReport == nil || !s.ImportReportReadConfigured {
		return false
	}
	if mode == contract.ImportContinueLive {
		return s.ImportProject != nil && s.ImportFinance != nil
	}
	return mode == contract.ImportArchiveOnly || mode == contract.ImportHistoricalClosed
}

// AvailableImportModes 返回当前配置可受理的新清单模式，不影响在途恢复。
// 参数：无。
// 返回值：公开模式列表，不含凭据或外部成功声明。
func (s *Service) AvailableImportModes() []string {
	modes := []string{"NORMAL"}
	for _, mode := range []contract.ImportMode{contract.ImportArchiveOnly, contract.ImportContinueLive, contract.ImportHistoricalClosed} {
		if s.importHistoryModeAvailable(mode) {
			modes = append(modes, string(mode))
		}
	}
	return modes
}
