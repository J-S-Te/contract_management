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
		if item.State != "COMPLETE" {
			index = i
			break
		}
	}
	if err == nil {
		err = s.checkBusinessLicense(run)
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
				current.Items[index].Issues = []contract.ImportIssue{{Field: "commit", Code: importErrorCode(err), Retryable: !errors.Is(err, ErrImportIdentityConflict) && !errors.Is(err, ErrImportConflict) && !errors.Is(err, ErrImportNeedsReview)}}
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
			current.State = "COMPLETED"
			if current.Confirmation != nil {
				current.State = "CONFIRMATION_PENDING"
			}
			current.SessionHash = nil
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

// executeImportItem 重校验冻结基线并持久建单，文件副本失败只恢复原合同。
// 参数：ctx及actor为重新验证的身份，b/item限定执行围栏。
// 返回值：完成或明确错误，不吞绑定及落库结果。
func (s *Service) executeImportItem(ctx context.Context, actor Principal, b contract.ImportBatch, item contract.ImportItemState) error {
	if item.State == "CORRECTION_REQUIRED" || item.State == "NEEDS_REVIEW" {
		return ErrImportNeedsReview
	}
	r, _ := s.importStore()
	id := item.ContractID
	if id == "" {
		c, digest, e := s.prepareImportItem(ctx, actor, b, item)
		if e != nil {
			return e
		}
		if digest != item.Digest {
			return ErrImportIdentityConflict
		}
		if e = s.EnsureOwnerNotFrozen(ctx, b.TenantID, actor.UserID); e != nil {
			return e
		}
		c.ID, c.TenantID, c.OwnerUserID, c.OwnerDisplayName = ulid.Make().String(), b.TenantID, actor.UserID, actor.DisplayName
		c.OwnerIdentityID, c.CreatedBy, c.Status = actor.IdentityID, actor.UserID, contract.StatusDraft
		c.OriginKind, c.ImportMode, c.OriginImportItemID = "NORMAL", contract.ImportNormal, item.ID
		if item.Item.Mode != contract.ImportNormal {
			owner, ownerErr := s.importHistoryOwner(ctx, actor, item.Item)
			if ownerErr != nil {
				return ownerErr
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
		id, e = r.CommitImportDraft(ctx, b.TenantID, b.ID, b.Revision, b.Epoch, item.ID, c, actor.UserID)
		if e != nil {
			return e
		}
	}
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
