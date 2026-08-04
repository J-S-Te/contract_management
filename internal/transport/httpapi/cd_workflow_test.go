package httpapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestContractCDSuppliesValidationOnlyProjectImageFallback(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}
	workflowPath := filepath.Join(filepath.Dir(filename), "..", "..", "..", ".github", "workflows", "ci-cd.yml")
	content, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read contract CD workflow: %v", err)
	}
	source := string(content)
	if !strings.Contains(source, "PROJECT_IMAGE=${project_image:-invalid.local/project-management@sha256:") {
		t.Fatal("contract CD does not provide the validation-only PROJECT_IMAGE fallback")
	}
	if !strings.Contains(source, "project_image=$(grep -m1 \"^PROJECT_IMAGE=\" .release.env | cut -d= -f2-)") {
		t.Fatal("contract CD does not preserve an existing released project image")
	}
	if !strings.Contains(source, "bash ./bin/deploy-service.sh contract %q") {
		t.Fatal("contract CD no longer invokes the contract-only deployment target")
	}
}
