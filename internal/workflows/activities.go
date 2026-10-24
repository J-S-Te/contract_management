package workflows

import (
	"context"
	"errors"

	core "github.com/J-S-Te/license-core"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/auditctx"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"go.temporal.io/sdk/temporal"
)

// ApprovalStore 负责事务持久化。由于 Temporal 活动
// 可能重试，其方法必须幂等。
type ApprovalStore interface {
	StartApproval(context.Context, StartApprovalActivityInput) error
	RecordCommand(context.Context, RecordCommandActivityInput) error
	CompleteApproval(context.Context, CompleteApprovalActivityInput) error
	CreateNotification(context.Context, NotifyActivityInput) error
	ArchiveExpired(context.Context, ExpiredArchiveInput) (ExpiredArchiveResult, error)
}

type LicenseChecker interface {
	Check(context.Context, core.Operation) error
}
type Activities struct {
	Store       ApprovalStore
	LicenseGate LicenseChecker
}

func (a *Activities) checkLicense(ctx context.Context) error {
	if a.LicenseGate == nil {
		return nil
	}
	if err := a.LicenseGate.Check(ctx, core.MUTATE_BUSINESS); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 通过持久化工作流定时器暂停，不能耗尽业务重试预算。
		return temporal.NewNonRetryableApplicationError("commercial license temporarily restricts business execution", commercialLicenseErrorType, nil)
	}
	return nil
}

func (a *Activities) StartApproval(ctx context.Context, in StartApprovalActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return criticalActivityError(a.Store.StartApproval(withAuditMeta(ctx, in.Audit), in))
}
func (a *Activities) RecordCommand(ctx context.Context, in RecordCommandActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return criticalActivityError(a.Store.RecordCommand(ctx, in))
}
func (a *Activities) CompleteApproval(ctx context.Context, in CompleteApprovalActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return criticalActivityError(a.Store.CompleteApproval(withAuditMeta(ctx, in.Audit), in))
}

// criticalActivityError 区分可重试执行故障与确定性业务冲突：版本/状态/找不到/
// 非法流转不应被无限重试，必须立即暴露给调用方；数据库/网络错误保持可重试。
func criticalActivityError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, apperrors.ErrNotFound) || errors.Is(err, apperrors.ErrVersionConflict) ||
		errors.Is(err, apperrors.ErrStateConflict) || errors.Is(err, contract.ErrInvalidTransition) ||
		errors.Is(err, contract.ErrInvalidStatus) {
		return temporal.NewNonRetryableApplicationError(err.Error(), "CONTRACT_BUSINESS_CONFLICT", err)
	}
	return err
}

// withAuditMeta 把工作流启动时携带的请求来源注入 Activity context，供事务内
// 审计 outbox 使用；异步链没有 HTTP context 可以自动继承。
func withAuditMeta(ctx context.Context, meta AuditMeta) context.Context {
	if meta.ClientIP == "" && meta.LoginIP == "" && meta.IPSource == "" && meta.RequestID == "" {
		return ctx
	}
	return auditctx.WithMeta(ctx, auditctx.Meta{
		LoginIP: meta.LoginIP, ClientIP: meta.ClientIP, IPSource: meta.IPSource, RequestID: meta.RequestID,
	})
}
func (a *Activities) CreateNotification(ctx context.Context, in NotifyActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return a.Store.CreateNotification(ctx, in)
}
func (a *Activities) ArchiveExpired(ctx context.Context, in ExpiredArchiveInput) (ExpiredArchiveResult, error) {
	if err := a.checkLicense(ctx); err != nil {
		return ExpiredArchiveResult{}, err
	}
	return a.Store.ArchiveExpired(ctx, in)
}
