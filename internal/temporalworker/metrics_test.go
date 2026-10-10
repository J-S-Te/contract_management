package temporalworker

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestWorkerReadinessTracksPollingAndShutdown(t *testing.T) {
	var ready atomic.Bool
	mux := workerMetricsMux(NewMetricsRegistry(), ready.Load)
	for _, state := range []bool{false, true, false} {
		ready.Store(state)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))
		want := http.StatusServiceUnavailable
		if state {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("ready=%v status=%d", state, response.Code)
		}
	}
}

func TestWorkerMetricsBindFailureIsReported(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := StartMetricsServerWithReadiness(context.Background(), listener.Addr().String(), NewMetricsRegistry(), logger, func() bool { return false }); err == nil {
		t.Fatal("occupied readiness port did not fail startup")
	}
}
