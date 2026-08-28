// Package temporalworker 封装合同子系统自己的 Temporal Worker 发布边界。
package temporalworker

import (
	"fmt"
	"strings"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// VersioningConfig 描述合同 Worker 的部署版本及默认工作流升级策略。
type VersioningConfig struct {
	Enabled        bool
	DeploymentName string
	BuildID        string
	Policy         string
}

// WorkerOptions 根据配置生成 Temporal Worker 选项。启用版本化时使用 Deployment-based
// Worker Versioning；关闭时保留旧 BuildID 行为，便于已有环境按明确开关滚动迁移。
func WorkerOptions(config VersioningConfig) (worker.Options, error) {
	deploymentName := strings.TrimSpace(config.DeploymentName)
	buildID := strings.TrimSpace(config.BuildID)
	if buildID == "" {
		return worker.Options{}, fmt.Errorf("Temporal worker build ID must not be empty")
	}
	options := worker.Options{DisableRegistrationAliasing: true, BuildID: buildID}
	if !config.Enabled {
		return options, nil
	}
	if deploymentName == "" {
		return worker.Options{}, fmt.Errorf("Temporal worker deployment name must not be empty")
	}
	behavior, err := versioningBehavior(config.Policy)
	if err != nil {
		return worker.Options{}, err
	}
	// DeploymentOptions.Version 生效后 SDK 会忽略旧 BuildID 字段；仍保留 BuildID 是为了
	// 关闭版本化开关时可以回退到旧路由，不需要在发布脚本中维护两套变量。
	options.DeploymentOptions = worker.DeploymentOptions{
		UseVersioning: true,
		Version: worker.WorkerDeploymentVersion{
			DeploymentName: deploymentName,
			BuildID:        buildID,
		},
		DefaultVersioningBehavior: behavior,
	}
	return options, nil
}

func versioningBehavior(policy string) (workflow.VersioningBehavior, error) {
	switch strings.ToUpper(strings.TrimSpace(policy)) {
	case "PINNED":
		return workflow.VersioningBehaviorPinned, nil
	case "AUTO_UPGRADE":
		return workflow.VersioningBehaviorAutoUpgrade, nil
	default:
		return workflow.VersioningBehaviorUnspecified, fmt.Errorf("unsupported Temporal worker versioning policy %q", policy)
	}
}
