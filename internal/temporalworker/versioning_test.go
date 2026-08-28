package temporalworker

import (
	"testing"

	"go.temporal.io/sdk/workflow"
)

func TestWorkerOptionsEnableDeploymentVersioning(t *testing.T) {
	options, err := WorkerOptions(VersioningConfig{Enabled: true, DeploymentName: "contract-management", BuildID: "contract-v2", Policy: "PINNED"})
	if err != nil {
		t.Fatalf("WorkerOptions() error = %v", err)
	}
	if !options.DeploymentOptions.UseVersioning || options.DeploymentOptions.Version.DeploymentName != "contract-management" || options.DeploymentOptions.Version.BuildID != "contract-v2" {
		t.Fatalf("deployment options = %#v", options.DeploymentOptions)
	}
	if options.DeploymentOptions.DefaultVersioningBehavior != workflow.VersioningBehaviorPinned {
		t.Fatalf("versioning behavior = %v", options.DeploymentOptions.DefaultVersioningBehavior)
	}
}

func TestWorkerOptionsRejectInvalidPolicy(t *testing.T) {
	if _, err := WorkerOptions(VersioningConfig{Enabled: true, DeploymentName: "contract-management", BuildID: "contract-v2", Policy: "latest"}); err == nil {
		t.Fatal("invalid versioning policy was accepted")
	}
}
