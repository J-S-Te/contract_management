package bootstrap

import (
	"context"
	"fmt"
	"github.com/J-S-Te/license-core/consumer"
	"log/slog"
	"os"
)

func StartCommercialLicense(ctx context.Context, logger *slog.Logger) (*consumer.Gate, error) {
	gate, err := consumer.FromEnvironment("contract_management")
	if err != nil {
		return nil, err
	}
	go func() {
		if err := gate.Run(ctx, func(error) { logger.Warn("commercial license synchronization unavailable") }); err != nil && ctx.Err() == nil {
			logger.Error("commercial license synchronization stopped")
		}
	}()
	return gate, nil
}

// RuntimeProcessMode prevents an independently enrolled API from spawning or
// acknowledging the Worker using the API's runtime credential.
func RuntimeProcessMode(component string) (bool, error) {
	return runtimeProcessMode(component, os.Getenv)
}

func runtimeProcessMode(component string, lookup func(string) string) (bool, error) {
	if component != "contract-api" && component != "contract-worker" {
		return false, fmt.Errorf("unsupported contract runtime component")
	}
	mode := lookup("CONTRACT_PROCESS_MODE")
	if mode == "" {
		mode = "legacy"
	}
	if mode != "legacy" && mode != "split" {
		return false, fmt.Errorf("invalid CONTRACT_PROCESS_MODE")
	}
	embedded := lookup("CONTRACT_RUN_WORKER_WITH_API")
	if embedded != "" && embedded != "true" && embedded != "false" {
		return false, fmt.Errorf("invalid CONTRACT_RUN_WORKER_WITH_API")
	}
	if mode == "split" && embedded == "true" {
		return false, fmt.Errorf("split contract runtime cannot start an embedded Worker")
	}
	if lookup("COMMERCIAL_LICENSE_ENABLED") == "true" {
		if mode != "split" || lookup("COMMERCIAL_LICENSE_SERVICE_ID") != component {
			return false, fmt.Errorf("commercial license component binding requires split process mode")
		}
	}
	return mode == "split", nil
}
