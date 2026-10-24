package bootstrap

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestEntrypointSelectsExactlyOneSplitProcess(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	entrypoint := filepath.Join(filepath.Dir(source), "..", "..", "docker-entrypoint.sh")
	for _, tt := range []struct {
		name, command, mode, embedded, enabled, service, want string
		fail                                                  bool
	}{
		{"API", "./api", "split", "false", "false", "", "api", false},
		{"Worker", "./worker", "split", "false", "false", "", "worker", false},
		{"legacy API compatibility", "./api", "legacy", "false", "false", "", "api", false},
		{"split double Worker rejected", "./api", "split", "true", "false", "", "", true},
		{"licensed legacy rejected", "./api", "legacy", "true", "true", "contract-api", "", true},
		{"Worker API credential rejected", "./worker", "split", "false", "true", "contract-api", "", true},
		{"API Worker credential rejected", "./api", "split", "false", "true", "contract-worker", "", true},
		{"licensed API", "./api", "split", "false", "true", "contract-api", "api", false},
		{"licensed Worker", "./worker", "split", "false", "true", "contract-worker", "worker", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, process := range []string{"api", "worker"} {
				if err := os.WriteFile(filepath.Join(dir, process), []byte("#!/bin/sh\necho "+process+"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", entrypoint, tt.command)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "CONTRACT_PROCESS_MODE=" + tt.mode, "CONTRACT_RUN_WORKER_WITH_API=" + tt.embedded, "COMMERCIAL_LICENSE_ENABLED=" + tt.enabled, "COMMERCIAL_LICENSE_SERVICE_ID=" + tt.service}
			output, err := cmd.Output()
			if (err != nil) != tt.fail || strings.TrimSpace(string(output)) != tt.want {
				t.Fatalf("output=%q error=%v", output, err)
			}
		})
	}
}
