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

// ContractImportConfirmationFreezer 在单事务内冻结历史确认，并把同一事务内读到的
// 租户自动审批运行开关交给调用方，使「策略判定」与「确认写入」原子一致。
//
// 为什么必须由仓储提供而不是复用 UpdateImportBatch：判定要读设置行，而设置行由
// 开关管理员独立写入。两条语句必须在同一事务内、且按「先批次行后设置行」的固定
// 顺序加锁，否则会与开关写入形成反向锁序。缺失该能力时提交按能力未开放失败关闭。
type ContractImportConfirmationFreezer interface {
	// FreezeImportConfirmation 锁定批次行后读取租户设置（无记录按零值），
	// 调用 change 修改批次并保存；change 返回错误时整体回滚。
	// 参数：ctx为上下文，tenantID/batchID定位台账，change为冻结回调。
	// 返回值：冲突、存储或回调错误。
	FreezeImportConfirmation(ctx context.Context, tenantID, batchID string, change func(*contract.ImportBatch, contract.ImportAutoApproveSetting) error) error
}

// SubmitImportConfirmation 冻结独立历史审批与异步原件建单身份。
//
// 策略判定（AUTO/MANUAL）在冻结事务内依据租户运行开关一次性固化：AUTO 直接落
// APPROVED 台账且不启动 Temporal（执行期由开立编排自动确认），MANUAL 保持既有
// STARTING + 工作流行为。判定一旦固化即成为本批次执行期的唯一口径。
//
// digest 在提交事务内随确认台账一起固化（对 {revision,items,sources} 求 SHA256），
// 客户端可用响应里的 confirmation.manifest_digest 对本次确认范围做独立复核。
//
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
		// AUTO 已冻结的确认没有工作流可启动（startImportConfirmation 内部早退），
		// 因此重放路径同样不需要 Temporal。
		return b, s.startImportConfirmation(ctx, b)
	}
	if !s.ContractImportHistoryEnabled || !s.ContractImportEnabled {
		return b, ErrImportUnsupported
	}
	// 事务外预读只为决定「本次是否需要 Temporal」：AUTO 路径不启工作流，
	// 因此缺 Temporal 不构成拒绝理由（真正的人工路径仍必须显式失败）。
	liveAuto, _, e := s.importAutoApproveLive(ctx, b.TenantID)
	if e != nil {
		return b, e
	}
	if s.Temporal == nil && !liveAuto {
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
	for _, item := range b.Items {
		issues := importFieldIssues(item.Item)
		// 历史开立通道的客户由编排先查重/创建，清单不声明 crm_customer_id 是正常输入。
		if s.importProvisionItemApplies(item.Item) {
			issues = withoutProvisionCustomerRequirement(issues)
		}
		if item.Item.Mode == contract.ImportNormal || contract.ValidateHistoryEvidence(item.Item) != nil || len(issues) > 0 || item.Digest == "" {
			return b, ErrValidation
		}
		if _, e = s.importHistoryOwner(ctx, actor, item.Item); e != nil {
			return b, e
		}
	}
	freezer, ok := s.Repo.(ContractImportConfirmationFreezer)
	if !ok {
		// 缺冻结能力时不能退化成「普通更新 + 执行期再读开关」：那会让判定与确认
		// 写入分处两个事务，重新制造本 P0 要消除的冲突窗口。
		return b, ErrImportUnsupported
	}
	digest, _ := contract.ImportDigest(struct {
		Revision uint64
		Items    []contract.ImportItemState
		Sources  []contract.ImportSource
	}{b.Revision, b.Items, b.Sources})
	now := time.Now().UTC()
	expected := b.Version
	e = freezer.FreezeImportConfirmation(ctx, b.TenantID, b.ID, func(current *contract.ImportBatch, setting contract.ImportAutoApproveSetting) error {
		if current.Version != expected || current.Revision != revision || current.Confirmation != nil {
			return ErrImportConflict
		}
		// 判定与确认写入同一事务：这里的 auto 就是本批次执行期的唯一口径。
		auto := s.ContractImportAutoApproveHistorical && setting.Enabled
		confirmation := &contract.ImportConfirmation{
			Revision:      revision,
			Digest:        digest,
			Roles:         roles,
			Commands:      []contract.ImportConfirmationCommand{},
			SubmittedAt:   now,
			ExpiresAt:     now.Add(72 * time.Hour),
			PolicyVersion: setting.Version,
		}
		if auto {
			// AUTO：台账即为最终批准形状，跳过人工会签与 Temporal。
			confirmation.PolicyDecision = contract.ImportPolicyDecisionAuto
			confirmation.WorkflowID = "contract-import-auto-" + b.ID
			confirmation.State = "APPROVED"
			confirmation.ApprovedRoles = append([]string(nil), roles...)
			confirmation.Assignees = map[string][]string{}
		} else {
			// MANUAL：保持既有语义——解析受派人，缺任一人即拒绝受理。
			confirmation.PolicyDecision = contract.ImportPolicyDecisionManual
			users, loadErr := s.loadPersonnel(ctx, actor, roles)
			if loadErr != nil {
				return loadErr
			}
			assigned := map[string][]string{}
			for _, role := range roles {
				assigned[role] = approversForRole(users, role)
				if len(assigned[role]) == 0 {
					return &ApprovalAssigneeUnavailableError{RoleCodes: []string{role}}
				}
			}
			confirmation.WorkflowID = "contract-import-confirmation-" + b.ID + "-" + strconv.FormatUint(revision, 10)
			confirmation.State = "STARTING"
			confirmation.Assignees = assigned
			confirmation.ApprovedRoles = []string{}
		}
		current.Confirmation = confirmation
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
//
// AUTO 确认在冻结时已批准，没有需要启动或恢复的工作流，直接视为成功；
// 这也是「自动审批提交不需要 Temporal」的唯一实现依据。
//
// 参数：ctx为上下文，b为持久化冻结台账。
// 返回值：启动成功、已存在或明确Temporal不可用错误。
func (s *Service) startImportConfirmation(ctx context.Context, b contract.ImportBatch) error {
	if b.Confirmation == nil {
		return ErrApprovalWorkflowUnavailable
	}
	if b.Confirmation.AutoApproved() {
		return nil
	}
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
//
// WITHDRAW 覆盖两种确认形态：人工会签（STARTING/PENDING）只追加命令，由工作流收敛；
// AUTO 已批准确认没有工作流，因此创建人在「零确认项且批次未完成」的严格护栏下撤回时，
// 由本方法在同一事务内直接落 WITHDRAWN + PAUSED + 升代，不需要等任何异步处理。
//
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
		// AUTO 确认在冻结时即 APPROVED 且没有工作流，永远不会经由
		// AdvanceImportConfirmation 收敛，因此「撤回」必须在本次事务内完成状态迁移，
		// 只追加命令等人处理会让错误提交永久卡死。
		// 护栏刻意保持严格：仅创建人、仅「一条清单项都没确认」且批次未完成时可撤回；
		// 已确认项意味着合同已取号归档，只能走更正流程，不能靠撤回抹掉事实。
		autoWithdraw := action == "WITHDRAW" && cf.AutoApproved() && cf.State == "APPROVED"
		if autoWithdraw {
			if b.State == "COMPLETED" {
				return ErrImportConflict
			}
			for _, item := range b.Items {
				if item.Confirmed {
					return ErrImportConflict
				}
			}
		}
		// AUTO 撤回不看确认有效期：AUTO 没有能走到 EXPIRED 的收敛路径，用有效期卡住
		// 只会让逃生口在冻结 72 小时后永久失效；MANUAL 的既有有效期语义不变。
		if b.Version != version || (cf.State != "PENDING" && !autoWithdraw && !(action == "WITHDRAW" && cf.State == "STARTING")) || (!autoWithdraw && !cf.ExpiresAt.After(time.Now())) {
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
		if autoWithdraw {
			// 镜像工作流撤回（AdvanceImportConfirmation 的 WITHDRAW 收敛）的效果：
			// 确认标记撤回、批次回到 PAUSED 并升代清租约，让在途的旧执行代次（含可能在途的
			// 自动确认）立即失效，交回创建人重新预检后提交。
			cf.State = "WITHDRAWN"
			b.State = "PAUSED"
			b.Epoch++
			b.LeaseUntil = time.Time{}
		}
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
