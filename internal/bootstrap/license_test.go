package bootstrap

import (
	"context"
	core "github.com/J-S-Te/license-core"
	"io"
	"log/slog"
	"testing"
)

func TestCommercialBootstrapCompatibilityAndConfiguredFailClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	t.Setenv("COMMERCIAL_LICENSE_ENABLED", "false")
	gate, err := StartCommercialLicense(ctx, logger)
	if err != nil || gate.Check(ctx, core.MUTATE_BUSINESS) != nil {
		t.Fatal("explicit compatibility startup failed")
	}
	t.Setenv("COMMERCIAL_LICENSE_ENABLED", "true")
	t.Setenv("COMMERCIAL_LICENSE_PLATFORM_PUBLIC_KEY_PATH", "")
	if _, err := StartCommercialLicense(ctx, logger); err == nil {
		t.Fatal("enabled missing trust failed open")
	}
}

func TestRuntimeProcessModeBindings(t *testing.T) {
	for _, tt := range []struct {
		name, mode, embedded, enabled, service, component string
		wantSplit, wantError                              bool
	}{
		{"legacy default", "", "true", "false", "", "contract-api", false, false},
		{"split API", "split", "false", "true", "contract-api", "contract-api", true, false},
		{"split Worker", "split", "false", "true", "contract-worker", "contract-worker", true, false},
		{"cross credential", "split", "false", "true", "contract-api", "contract-worker", false, true},
		{"licensed legacy", "legacy", "false", "true", "contract-api", "contract-api", false, true},
		{"double Worker", "split", "true", "false", "", "contract-api", false, true},
		{"unknown mode", "unknown", "false", "false", "", "contract-api", false, true},
		{"bad boolean", "split", "yes", "false", "", "contract-api", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]string{"CONTRACT_PROCESS_MODE": tt.mode, "CONTRACT_RUN_WORKER_WITH_API": tt.embedded, "COMMERCIAL_LICENSE_ENABLED": tt.enabled, "COMMERCIAL_LICENSE_SERVICE_ID": tt.service}
			got, err := runtimeProcessMode(tt.component, func(key string) string { return values[key] })
			if (err != nil) != tt.wantError || got != tt.wantSplit {
				t.Fatalf("split=%v err=%v", got, err)
			}
		})
	}
}
