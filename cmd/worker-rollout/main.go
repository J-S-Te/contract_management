// Command worker-rollout 以显式动作控制合同 Worker Deployment 的灰度和提升。
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/temporalworker"
	"go.temporal.io/sdk/client"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Temporal worker rollout failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	address := strings.TrimSpace(os.Getenv("TEMPORAL_ADDRESS"))
	namespace := strings.TrimSpace(os.Getenv("TEMPORAL_NAMESPACE"))
	deployment := strings.TrimSpace(os.Getenv("TEMPORAL_WORKER_DEPLOYMENT_NAME"))
	buildID := strings.TrimSpace(os.Getenv("TEMPORAL_WORKER_BUILD_ID"))
	action := strings.ToUpper(strings.TrimSpace(os.Getenv("TEMPORAL_WORKER_ROLLOUT_ACTION")))
	identity := strings.TrimSpace(os.Getenv("TEMPORAL_WORKER_ROLLOUT_IDENTITY"))
	if address == "" || namespace == "" || deployment == "" || buildID == "" || identity == "" {
		return fmt.Errorf("Temporal address, namespace, deployment, build ID and rollout identity are required")
	}
	options := client.Options{HostPort: address, Namespace: namespace}
	tlsEnabled, err := strconv.ParseBool(valueOrDefault("TEMPORAL_TLS", "false"))
	if err != nil {
		return fmt.Errorf("TEMPORAL_TLS must be boolean: %w", err)
	}
	if tlsEnabled {
		options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		options.ConnectionOptions.TLSDisabled = true
	}
	if key := strings.TrimSpace(os.Getenv("TEMPORAL_API_KEY")); key != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	temporalClient, err := client.DialContext(ctx, options)
	if err != nil {
		return fmt.Errorf("connect Temporal control plane: %w", err)
	}
	defer temporalClient.Close()
	handle := temporalClient.WorkerDeploymentClient().GetHandle(deployment)
	switch action {
	case "RAMP":
		percentage, parseErr := strconv.ParseFloat(valueOrDefault("TEMPORAL_WORKER_RAMP_PERCENTAGE", "5"), 32)
		if parseErr != nil {
			return fmt.Errorf("TEMPORAL_WORKER_RAMP_PERCENTAGE must be numeric: %w", parseErr)
		}
		return temporalworker.RampVersion(ctx, handle, buildID, identity, float32(percentage))
	case "PROMOTE":
		return temporalworker.PromoteCurrent(ctx, handle, buildID, identity)
	case "ABORT_RAMP":
		return temporalworker.RampVersion(ctx, handle, "", identity, 0)
	default:
		return fmt.Errorf("TEMPORAL_WORKER_ROLLOUT_ACTION must be RAMP, PROMOTE or ABORT_RAMP")
	}
}

func valueOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
