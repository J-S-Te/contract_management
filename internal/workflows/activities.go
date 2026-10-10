package workflows

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"go.temporal.io/sdk/temporal"
)

// ApprovalStore owns transactional persistence. Its methods must be
// idempotent because Temporal Activities can be retried.
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
		// Pause in the durable workflow timer, not exhaust a business retry budget.
		return temporal.NewNonRetryableApplicationError("commercial license temporarily restricts business execution", commercialLicenseErrorType, nil)
	}
	return nil
}

func (a *Activities) StartApproval(ctx context.Context, in StartApprovalActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return a.Store.StartApproval(ctx, in)
}
func (a *Activities) RecordCommand(ctx context.Context, in RecordCommandActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return a.Store.RecordCommand(ctx, in)
}
func (a *Activities) CompleteApproval(ctx context.Context, in CompleteApprovalActivityInput) error {
	if err := a.checkLicense(ctx); err != nil {
		return err
	}
	return a.Store.CompleteApproval(ctx, in)
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
