package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

type integrationOutboxRepositoryStub struct {
	application.Repository
	failed      map[string][]application.FailedIntegrationEvent
	retryFound  bool
	retried     []string
	listTenant  string
	retryTenant string
	retryActor  string
}

func (s *integrationOutboxRepositoryStub) ListFailedIntegrationEvents(_ context.Context, tenantID, kind string, limit int) ([]application.FailedIntegrationEvent, error) {
	s.listTenant = tenantID
	return s.failed[kind], nil
}

func (s *integrationOutboxRepositoryStub) RetryFailedIntegrationEvent(_ context.Context, tenantID, kind, id, actorID string) (bool, error) {
	s.retryTenant, s.retryActor = tenantID, actorID
	s.retried = append(s.retried, kind+"/"+id)
	return s.retryFound, nil
}

func integrationOutboxIdentity(permissions map[string]bool) Identity {
	return identityFunc(func(context.Context, *http.Request) (application.Principal, error) {
		return application.Principal{TenantID: "tenant-1", UserID: "ops-1", Permissions: permissions}, nil
	})
}

func TestIntegrationOutboxListsAllFourDeadKinds(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	repository := &integrationOutboxRepositoryStub{failed: map[string][]application.FailedIntegrationEvent{
		application.IntegrationOutboxFinancial: {{
			ID: "fin-1", Kind: application.IntegrationOutboxFinancial, AggregateID: "contract-1",
			Event: "contract.financial_effective.v1", Attempts: 20, LastError: "settlement 503",
			CreatedAt: now, UpdatedAt: now,
		}},
		application.IntegrationOutboxResponsibility: {{
			ID: "resp-1", Kind: application.IntegrationOutboxResponsibility, AggregateID: "contract-2",
			Event: "contract.responsibility_changed.v1", Attempts: 20, CreatedAt: now, UpdatedAt: now,
		}},
		application.IntegrationOutboxProject: {{
			ID: "proj-1", Kind: application.IntegrationOutboxProject, AggregateID: "contract-3",
			Event: "contract.activation", Attempts: 20, CreatedAt: now, UpdatedAt: now,
		}},
		application.IntegrationOutboxLink: {{
			ID: "link-1", Kind: application.IntegrationOutboxLink, AggregateID: "intake-1",
			Event: "crm.opportunity_contract_link", Attempts: 20, CreatedAt: now, UpdatedAt: now,
		}},
	}}
	router := NewRouter(&application.Service{Repo: repository}, integrationOutboxIdentity(map[string]bool{"approval.manage": true}), nil)
	for _, kind := range application.IntegrationOutboxKinds() {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/integration-outbox/failed?kind="+kind, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", kind, response.Code, response.Body.String())
		}
		var parsed struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Data.Items) != 1 || parsed.Data.Items[0]["kind"] != kind ||
			parsed.Data.Items[0]["aggregate_id"] == "" || parsed.Data.Items[0]["event"] == "" ||
			parsed.Data.Items[0]["updated_at"] == "" {
			t.Fatalf("%s items=%+v body=%s", kind, parsed.Data.Items, response.Body.String())
		}
	}
	if repository.listTenant != "tenant-1" {
		t.Fatalf("list tenant=%q", repository.listTenant)
	}
}

func TestIntegrationOutboxRejectsUnknownKindAndMissingPermission(t *testing.T) {
	repository := &integrationOutboxRepositoryStub{}
	router := NewRouter(&application.Service{Repo: repository}, integrationOutboxIdentity(map[string]bool{"approval.manage": true}), nil)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/integration-outbox/failed?kind=unknown", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown kind status=%d body=%s", response.Code, response.Body.String())
	}

	forbidden := NewRouter(&application.Service{Repo: repository}, integrationOutboxIdentity(map[string]bool{"contract.read": true}), nil)
	request = httptest.NewRequest(http.MethodGet, "/api/v1/integration-outbox/failed?kind=financial", nil)
	response = httptest.NewRecorder()
	forbidden.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing permission status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestIntegrationOutboxRetryResetsAndReportsNotFound(t *testing.T) {
	repository := &integrationOutboxRepositoryStub{retryFound: true}
	router := NewRouter(&application.Service{Repo: repository}, integrationOutboxIdentity(map[string]bool{"approval.manage": true}), nil)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/integration-outbox/failed/financial/fin-1/retry", nil)
	request.Header.Set("Origin", "http://example.com")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", response.Code, response.Body.String())
	}
	if len(repository.retried) != 1 || repository.retried[0] != "financial/fin-1" ||
		repository.retryTenant != "tenant-1" || repository.retryActor != "ops-1" {
		t.Fatalf("retried=%v tenant=%q actor=%q", repository.retried, repository.retryTenant, repository.retryActor)
	}

	repository.retryFound = false
	request = httptest.NewRequest(http.MethodPost, "/api/v1/integration-outbox/failed/link/missing/retry", nil)
	request.Header.Set("Origin", "http://example.com")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing retry status=%d body=%s", response.Code, response.Body.String())
	}
}
