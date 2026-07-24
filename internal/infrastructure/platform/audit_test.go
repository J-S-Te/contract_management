package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditClientUsesClientCredentialsAndContractHeaders(t *testing.T) {
	var event map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			user, password, ok := r.BasicAuth()
			require.True(t, ok)
			require.Equal(t, "client", user)
			require.Equal(t, "secret", password)
			require.Equal(t, "client_credentials", r.FormValue("grant_type"))
			require.Equal(t, "audit.ingest", r.FormValue("scope"))
			_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer","expires_in":3600,"scope":"audit.ingest"}`))
		case "/api/v1/audit/events":
			require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
			require.Equal(t, "request-1", r.Header.Get("X-Request-ID"))
			require.Equal(t, "correlation-1", r.Header.Get("X-Correlation-ID"))
			require.Regexp(t, `^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`, r.Header.Get("traceparent"))
			require.NoError(t, json.NewDecoder(r.Body).Decode(&event))
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"code":"OK"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	reporter := NewAuditReporter(server.URL, "client", "secret", "contract_management", "dev")
	require.NotNil(t, reporter)
	require.NoError(t, reporter.Report(context.Background(), AuditEvent{ActorID: "user-1", Action: "CONTRACT_MANAGEMENT:POST", ResourceType: "CONTRACT", ResourceID: "contract-1", RequestID: "request-1", CorrelationID: "correlation-1", Result: "SUCCESS", ReasonCode: "201"}))
	require.Equal(t, "contract_management", event["application_code"])
	require.Equal(t, "dev", event["environment_code"])
	require.Equal(t, "request-1", event["request_id"])
	require.Equal(t, "USER", event["actor_type"])
}

func TestAuditReporterIsDisabledWithoutCompleteCredentials(t *testing.T) {
	require.Nil(t, NewAuditReporter("http://platform", "client", "", "contract_management", "dev"))
}
