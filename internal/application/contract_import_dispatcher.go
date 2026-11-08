package application

import (
	"context"
	"errors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/oklog/ulid/v2"
	"log/slog"
	"time"
)

// RunImportDispatcher 在API进程中恢复持久任务，不依赖浏览器连接存活。
// 参数：ctx控制停止；每次只处理一个持租约单项，不保存用户明文令牌。
// 返回值：无；基础设施错误仅记录不含业务正文的分类日志。
func (s *Service) RunImportDispatcher(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	cleanupAt := time.Now().Add(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().After(cleanupAt) {
				cleanupAt = time.Now().Add(time.Minute)
				if e := s.CleanupImportSources(ctx); e != nil {
					slog.ErrorContext(ctx, "contract import staging cleanup failed", "error", e)
				}
			}
			if e := s.DispatchImportTargetOnce(ctx); e != nil {
				slog.ErrorContext(ctx, "contract import target dispatch failed", "error", e)
			}
			if e := s.DispatchImportOnce(ctx); e != nil {
				slog.ErrorContext(ctx, "contract import dispatch failed", "error", e)
			}
		}
	}
}

// DispatchImportOnce 认领一个批次并执行一个原子单项。
// 参数：ctx控制后台调用和超时。
// 返回值：台账或基础设施错误；业务失败保留在逐项结果。
func (s *Service) DispatchImportOnce(ctx context.Context) error {
	r, e := s.importStore()
	if e != nil {
		return e
	}
	if recovery, ok := r.(interface {
		ListStartingImportConfirmations(context.Context) ([]contract.ImportBatch, error)
	}); ok {
		batches, err := recovery.ListStartingImportConfirmations(ctx)
		if err != nil {
			return err
		}
		for _, batch := range batches {
			if err = s.startImportConfirmation(ctx, batch); err != nil {
				return err
			}
		}
	}
	b, found, e := r.ClaimImportBatch(ctx, time.Now().UTC())
	if e != nil || !found {
		return e
	}
	run, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	actor, err := Principal{}, ErrForbidden
	if s.ImportActorResolver != nil {
		actor, err = s.ImportActorResolver(run, b.SessionHash, b.TenantID, b.ActorID)
	}
	if err == nil {
		err = importAccess(actor, b, "contract.import.commit")
	}
	index := -1
	for i, item := range b.Items {
		// 跳过已完成与不可自动推进的终态行：一个不可重试的 BLOCKED 行
		// 不能把整批卡在同一位置上无限重试。
		if item.State == "COMPLETE" || importItemTerminal(item) {
			continue
		}
		index = i
		break
	}
	if err == nil {
		err = s.checkBusinessLicense(run)
	}
	if err == nil && b.Confirmation != nil && b.Confirmation.AutoApproved() && !s.importAutoApproveBatchEligible(b) {
		err = ErrImportConflict
	}
	if err == nil && index >= 0 {
		err = s.executeImportItem(run, actor, b, b.Items[index])
	}
	// 请求取消后仍需短暂独立写入已知失败；若数据库不可用则等待租约重领。
	finish, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelFinish()
	return r.UpdateImportBatch(finish, b.TenantID, b.ID, func(current *contract.ImportBatch) error {
		if current.Epoch != b.Epoch || current.State != "RUNNING" {
			return ErrImportConflict
		}
		current.LeaseUntil = time.Time{}
		current.State = "QUEUED"
		if err != nil {
			current.State = "PARTIAL"
			if index >= 0 {
				current.Items[index].State = "BLOCKED"
				if errors.Is(err, ErrImportNeedsReview) {
					current.Items[index].State = "NEEDS_REVIEW"
				}
				current.Items[index].Issues = []contract.ImportIssue{importItemIssue(err)}
			}
			return nil
		}
		complete := true
		for _, item := range current.Items {
			if item.State != "COMPLETE" {
				complete = false
			}
		}
		if complete {
			// 全部开立完成后的收敛：已获批（含自动审批写入的合成确认）说明逐目标
			// 基线已冻结，批次进入 TARGETS_PENDING 由目标投递器接手；仍待会签的
			// 保持 CONFIRMATION_PENDING；没有确认台账（普通导入）仍直接 COMPLETED。
			switch {
			case current.Confirmation != nil && current.Confirmation.State == "APPROVED":
				current.State = "TARGETS_PENDING"
			case current.Confirmation != nil:
				current.State = "CONFIRMATION_PENDING"
			default:
				current.State = "COMPLETED"
			}
			current.SessionHash = nil
			return nil
		}
		if index < 0 {
			// 剩余项全部是不可自动推进的终态（如不可重试的 BLOCKED）：
			// 批次转 PARTIAL 并停止重排 QUEUED，否则调度器会每秒空转重领同一批；
			// 操作者更正数据或恢复（resume）后再推进。
			current.State = "PARTIAL"
		}
		return nil
	})
}

// importErrorCode 将后台错误转换为可恢复且不泄露业务数据的稳定类别。
// 参数：err为执行错误。
// 返回值：公开错误码。
func importErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrCRMActorContextRequired):
		return "IMPORT_CRM_SESSION_REQUIRED"
	case errors.Is(err, ErrForbidden):
		return "IMPORT_AUTHORIZATION_REQUIRED"
	case errors.Is(err, ErrValidation):
		return "IMPORT_VALIDATION_FAILED"
	case errors.Is(err, ErrImportIdentityConflict):
		return "IMPORT_SOURCE_ID_CONFLICT"
	case errors.Is(err, ErrImportConflict):
		return "IMPORT_REVISION_CONFLICT"
	case errors.Is(err, ErrImportNeedsReview):
		return "IMPORT_NEEDS_REVIEW"
	case errors.Is(err, ErrImportUnsupported):
		return "IMPORT_POLICY_UNSUPPORTED"
	case errors.Is(err, ErrCommercialLicense):
		return "COMMERCIAL_LICENSE_RESTRICTED"
	default:
		return "IMPORT_DEPENDENCY_UNAVAILABLE"
	}
}

// importItemIssue 把一次后台执行失败映射为逐项 Issue。
// 参数：err 为执行错误。
// 返回值：历史开立失败按设计定稿 3.3 的五个错误码落 Issue（字段路径与可重试标记
// 由分类结果给出）；来源身份冲突额外带出既有合同/导入项与冲突类别；
// 其余错误保持原有类别与可重试判定。
func importItemIssue(err error) contract.ImportIssue {
	var provision *importProvisionFailure
	if errors.As(err, &provision) {
		return provision.issue()
	}
	if issue, conflict := importIdentityConflictIssue(err, "commit"); conflict {
		return issue
	}
	return contract.ImportIssue{Field: "commit", Code: importErrorCode(err),
		Retryable: !errors.Is(err, ErrImportIdentityConflict) && !errors.Is(err, ErrImportConflict) && !errors.Is(err, ErrImportNeedsReview)}
}

// importItemTerminal 判断单项是否已处于不可自动推进的终态。
// 只有「需要人工纠错或来源身份重建」的项才算终态：纠错/待复核状态，或身份冲突类
// BLOCKED。围栏冲突（IMPORT_REVISION_CONFLICT）、权限、校验、依赖类 BLOCKED 仍可
// 通过 resume/重新预检恢复，不能因 Retryable=false 就永久锁死。
// 参数：item 为批次中的单项。
// 返回值：需业务更正/复核或来源身份重建时返回 true。
func importItemTerminal(item contract.ImportItemState) bool {
	switch item.State {
	case "CORRECTION_REQUIRED", "NEEDS_REVIEW":
		return true
	case "BLOCKED":
		for _, issue := range item.Issues {
			switch issue.Code {
			case "IMPORT_SOURCE_ID_CONFLICT", "IMPORT_CORRECTION_REQUIRED", "IMPORT_NEEDS_REVIEW":
				return true
			}
		}
	}
	return false
}

// importItemPreparer 是执行期真正准备合同的函数（普通导入复用普通准备，
// 历史开立复用开立准备），签名与 Service.prepareImportItem 一致。
type importItemPreparer func(context.Context, Principal, contract.ImportBatch, contract.ImportItemState) (contract.Contract, string, error)

// executeImportItem 重校验冻结基线并持久建单，文件副本失败只恢复原合同。
// 参数：ctx及actor为重新验证的身份，b/item限定执行围栏。
// 返回值：完成或明确错误，不吞绑定及落库结果。
func (s *Service) executeImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState) error {
	if item.State == "CORRECTION_REQUIRED" || item.State == "NEEDS_REVIEW" {
		return ErrImportNeedsReview
	}
	// 历史开立通道（仅 ARCHIVE_ONLY / HISTORICAL_CLOSED 且开关开启）走独立的
	// 五步编排：解析销售 → 查重/建客户 → 建合同 → 建商机并签约 → 回写。
	// NORMAL 与关闭态完全不进入该分支，行为与既有链路一致。
	// 已按开立通道预检过的行（NEEDS_PROVISION）即使开关随后关闭也必须走这里，
	// 才能给出 IMPORT_CRM_PROVISION_DISABLED 而不是含混的校验失败。
	if s.importProvisionItemApplies(item.Item) ||
		(importProvisionModeApplies(item.Item.Mode) && item.State == contract.ImportItemStateNeedsProvision) {
		return s.executeProvisionImportItem(ctx, actor, b, item)
	}
	id := item.ContractID
	if id == "" {
		created, e := s.commitImportContract(ctx, actor, b, item, s.prepareImportItem, nil)
		if e != nil {
			return e
		}
		id = created
	}
	return s.finishImportItem(ctx, actor, b, item, id, nil)
}

// commitImportContract 在冻结基线内创建一条导入合同（含来源身份与单项结果）。
// 参数：actor 为重新验证的身份，b/item 限定执行围栏，prepare 为准备函数，
// adjust 在摘要校验通过后补齐仅由开立通道提供的权威字段（crm_customer_id / 客户名）。
// 返回值：稳定合同 ID；摘要不符、围栏失效或创建失败均返回错误。
func (s *Service) commitImportContract(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState, prepare importItemPreparer, adjust func(*contract.Contract) error) (string, error) {
	r, err := s.importStore()
	if err != nil {
		return "", err
	}
	c, digest, err := prepare(ctx, actor, b, item)
	if err != nil {
		return "", err
	}
	if digest != item.Digest {
		return "", &ImportIdentityConflictError{Kind: ImportConflictKindBaselineDrift}
	}
	if err = s.EnsureOwnerNotFrozen(ctx, b.TenantID, actor.UserID); err != nil {
		return "", err
	}
	c.ID, c.TenantID, c.OwnerUserID, c.OwnerDisplayName = ulid.Make().String(), b.TenantID, actor.UserID, actor.DisplayName
	c.OwnerIdentityID, c.CreatedBy, c.Status = actor.IdentityID, actor.UserID, contract.StatusDraft
	c.OriginKind, c.ImportMode, c.OriginImportItemID = "NORMAL", contract.ImportNormal, item.ID
	if item.Item.Mode != contract.ImportNormal {
		owner, ownerErr := s.importHistoryOwner(ctx, actor, item.Item)
		if ownerErr != nil {
			return "", ownerErr
		}
		c.OwnerUserID, c.OwnerDisplayName, c.OwnerIdentityID = owner.UserID, owner.DisplayName, owner.UserID
		c.OriginKind, c.ImportMode, c.ImportPolicy = "IMPORTED", item.Item.Mode, item.Item.Policy
		evidence := item.Item.HistoryEvidence
		c.HistoricalStatus, c.HistoricalCustomerName, c.HistoricalOwnerLabel = evidence.HistoricalStatus, evidence.HistoricalCustomerName, evidence.HistoricalOwnerLabel
	}
	c.ContentHash, c.SourceFileStatus = c.SourceSHA256, "PENDING"
	for _, src := range b.Sources {
		if src.ID == item.Item.SourceFileRef {
			c.SourceFileID = src.GatewayID
		}
	}
	if adjust != nil {
		if err = adjust(&c); err != nil {
			return "", err
		}
	}
	return r.CommitImportDraft(ctx, b.TenantID, b.ID, b.Revision, b.Epoch, item.ID, c, actor.UserID)
}

// finishImportItem 把已创建的合同接入源文件网关，再回写单项结果。
// 参数：actor 为执行身份，b/item 为围栏，id 为合同 ID，
// beforeComplete 在标记 COMPLETE 之前执行（历史开立的商机编排挂在这里，
// 从而保证商机失败时该行保持 BLOCKED 且可重试）。
// 返回值：完成或明确错误。
func (s *Service) finishImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState, id string, beforeComplete func(contract.Contract) error) error {
	c, e := s.Repo.GetContract(ctx, b.TenantID, id)
	if e != nil {
		return e
	}
	if c.Version != 1 {
		return ErrImportNeedsReview
	}
	if c.CreatedBy != actor.UserID {
		return ErrForbidden
	}
	_, name, _ := sourceGatewayMetadata(c)
	if e = s.StampedFileGateway.Bind(ctx, s.StampedFileApplicationID, c.SourceFileID, "contract", id, "EXTERNAL_SOURCE", name); e != nil {
		return e
	}
	state, ok := s.Repo.(SourceFileGatewayStateRepository)
	if !ok {
		return ErrImportUnsupported
	}
	if e = state.MarkContractSourceGatewayResult(ctx, b.TenantID, id, c.SourceFileID, "READY", ""); e != nil {
		return e
	}
	if beforeComplete != nil {
		if e = beforeComplete(c); e != nil {
			return e
		}
	}
	return s.markImportItemComplete(ctx, b, item)
}

// markImportItemComplete 在批次围栏内把单项结果标记为 COMPLETE。
// 参数：ctx 为请求，b 为执行围栏，item 为目标单项。
// 返回值：围栏失效返回 ErrImportConflict，其余为存储错误。
func (s *Service) markImportItemComplete(ctx context.Context, b contract.ImportBatch, item contract.ImportItemState) error {
	r, e := s.importStore()
	if e != nil {
		return e
	}
	return r.UpdateImportBatch(ctx, b.TenantID, b.ID, func(current *contract.ImportBatch) error {
		if current.Epoch != b.Epoch || current.State != "RUNNING" {
			return ErrImportConflict
		}
		for i := range current.Items {
			if current.Items[i].ID == item.ID {
				current.Items[i].State = "COMPLETE"
				current.Items[i].Issues = nil
				return nil
			}
		}
		return ErrImportConflict
	})
}
