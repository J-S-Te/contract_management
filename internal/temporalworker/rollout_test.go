package temporalworker

import (
	"context"
	"testing"

	"go.temporal.io/sdk/client"
)

type deploymentHandleStub struct {
	current client.WorkerDeploymentSetCurrentVersionOptions
	ramping client.WorkerDeploymentSetRampingVersionOptions
}

func (stub *deploymentHandleStub) Describe(context.Context, client.WorkerDeploymentDescribeOptions) (client.WorkerDeploymentDescribeResponse, error) {
	return client.WorkerDeploymentDescribeResponse{ConflictToken: []byte("version-1")}, nil
}
func (stub *deploymentHandleStub) SetCurrentVersion(_ context.Context, options client.WorkerDeploymentSetCurrentVersionOptions) (client.WorkerDeploymentSetCurrentVersionResponse, error) {
	stub.current = options
	return client.WorkerDeploymentSetCurrentVersionResponse{}, nil
}
func (stub *deploymentHandleStub) SetRampingVersion(_ context.Context, options client.WorkerDeploymentSetRampingVersionOptions) (client.WorkerDeploymentSetRampingVersionResponse, error) {
	stub.ramping = options
	return client.WorkerDeploymentSetRampingVersionResponse{}, nil
}

func TestPromoteCurrentUsesConflictTokenAndPollerProtection(t *testing.T) {
	stub := &deploymentHandleStub{}
	if err := PromoteCurrent(context.Background(), stub, "contract-v2", "deploy-42"); err != nil {
		t.Fatalf("PromoteCurrent() error = %v", err)
	}
	if stub.current.BuildID != "contract-v2" || string(stub.current.ConflictToken) != "version-1" || stub.current.AllowNoPollers || stub.current.IgnoreMissingTaskQueues {
		t.Fatalf("current options = %#v", stub.current)
	}
}

func TestRampVersionRejectsInvalidPercentage(t *testing.T) {
	if err := RampVersion(context.Background(), &deploymentHandleStub{}, "contract-v2", "deploy-42", 101); err == nil {
		t.Fatal("invalid ramp percentage was accepted")
	}
}
