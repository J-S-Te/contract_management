package workflows

import "context"

// ApprovalStore owns transactional persistence. Its methods must be
// idempotent because Temporal Activities can be retried.
type ApprovalStore interface {
	StartApproval(context.Context, StartApprovalActivityInput) error
	RecordCommand(context.Context, RecordCommandActivityInput) error
	CompleteApproval(context.Context, CompleteApprovalActivityInput) error
	CreateNotification(context.Context, NotifyActivityInput) error
	ArchiveExpired(context.Context, ExpiredArchiveInput) (ExpiredArchiveResult, error)
}

type Activities struct{ Store ApprovalStore }

func (a *Activities) StartApproval(ctx context.Context, in StartApprovalActivityInput) error {
	return a.Store.StartApproval(ctx, in)
}
func (a *Activities) RecordCommand(ctx context.Context, in RecordCommandActivityInput) error {
	return a.Store.RecordCommand(ctx, in)
}
func (a *Activities) CompleteApproval(ctx context.Context, in CompleteApprovalActivityInput) error {
	return a.Store.CompleteApproval(ctx, in)
}
func (a *Activities) CreateNotification(ctx context.Context, in NotifyActivityInput) error {
	return a.Store.CreateNotification(ctx, in)
}
func (a *Activities) ArchiveExpired(ctx context.Context, in ExpiredArchiveInput) (ExpiredArchiveResult, error) {
	return a.Store.ArchiveExpired(ctx, in)
}
