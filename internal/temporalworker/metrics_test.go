package temporalworker

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsRegistryExposesWorkflowFailures(t *testing.T) {
	registry := NewMetricsRegistry()
	handler := registry.WithTags(map[string]string{"workflow_type": "ContractApproval"})
	handler.Counter("temporal_workflow_failed").Inc(2)
	handler.Timer("temporal_workflow_endtoend_latency").Record(1500 * time.Millisecond)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	body := response.Body.String()
	if !strings.Contains(body, `temporal_workflow_failed_total{workflow_type="ContractApproval"} 2`) ||
		!strings.Contains(body, `temporal_workflow_endtoend_latency_count{workflow_type="ContractApproval"} 1`) {
		t.Fatalf("metrics body = %s", body)
	}
}
