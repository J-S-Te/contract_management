package workflows

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	"sync/atomic"
	"testing"
)

type restrictedLicense struct{}

func (restrictedLicense) Check(context.Context, core.Operation) error { return core.ErrDenied }

func TestActivitiesDenyBeforeAnyStoreMutation(t *testing.T) {
	a := &Activities{LicenseGate: restrictedLicense{}} // nil Store would panic on bypass
	ctx := context.Background()
	for _, f := range []func() error{
		func() error { return a.StartApproval(ctx, StartApprovalActivityInput{}) },
		func() error { return a.RecordCommand(ctx, RecordCommandActivityInput{}) },
		func() error { return a.CompleteApproval(ctx, CompleteApprovalActivityInput{}) },
		func() error { return a.CreateNotification(ctx, NotifyActivityInput{}) },
		func() error { _, err := a.ArchiveExpired(ctx, ExpiredArchiveInput{}); return err },
	} {
		err := f()
		appErr, ok := err.(*temporal.ApplicationError)
		if !ok || appErr.Type() != commercialLicenseErrorType || !appErr.NonRetryable() {
			t.Fatalf("denial=%v", err)
		}
	}
}

func TestLicensedActivityPausesThenResumesWithoutLosingWorkflow(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var attempts atomic.Int32
	env.RegisterActivityWithOptions(func(context.Context, string) error {
		if attempts.Add(1) <= 10 {
			return temporal.NewNonRetryableApplicationError("restricted", commercialLicenseErrorType, nil)
		}
		return nil
	}, activity.RegisterOptions{Name: "license-test"})
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		return executeLicensedActivity(ctx, activityContext(ctx), "license-test", "input", nil)
	})
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil || attempts.Load() != 11 {
		t.Fatalf("attempts=%d err=%v", attempts.Load(), env.GetWorkflowError())
	}
}
