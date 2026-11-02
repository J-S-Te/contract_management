package application

import (
	"context"
	"errors"
	"testing"
)

type integrationOutboxRepoStub struct {
	Repository
	listed  []FailedIntegrationEvent
	kind    string
	limit   int
	tenant  string
	found   bool
	retried bool
	actor   string
}

func (s *integrationOutboxRepoStub) ListFailedIntegrationEvents(_ context.Context, tenantID, kind string, limit int) ([]FailedIntegrationEvent, error) {
	s.tenant, s.kind, s.limit = tenantID, kind, limit
	return s.listed, nil
}

func (s *integrationOutboxRepoStub) RetryFailedIntegrationEvent(_ context.Context, tenantID, kind, id, actorID string) (bool, error) {
	s.retried = true
	s.actor = actorID
	return s.found, nil
}

func managerPrincipal() Principal {
	return Principal{TenantID: "tenant-1", UserID: "ops-1", Permissions: map[string]bool{IntegrationOutboxManagerPermission: true}}
}

func TestServiceIntegrationOutboxValidatesKindLimitAndPermission(t *testing.T) {
	repository := &integrationOutboxRepoStub{}
	service := &Service{Repo: repository}
	if _, err := service.ListFailedIntegrationEvents(context.Background(), managerPrincipal(), "unknown", 10); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown kind err=%v", err)
	}
	if _, err := service.ListFailedIntegrationEvents(context.Background(), Principal{TenantID: "tenant-1"}, IntegrationOutboxFinancial, 10); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing permission err=%v", err)
	}
	// limit 超界收敛到上限，交给仓储执行。
	if _, err := service.ListFailedIntegrationEvents(context.Background(), managerPrincipal(), IntegrationOutboxProject, 5000); err != nil {
		t.Fatal(err)
	}
	if repository.limit != 100 || repository.kind != IntegrationOutboxProject || repository.tenant != "tenant-1" {
		t.Fatalf("repo call kind=%q limit=%d tenant=%q", repository.kind, repository.limit, repository.tenant)
	}
	if _, err := service.RetryFailedIntegrationEvent(context.Background(), managerPrincipal(), IntegrationOutboxLink, " "); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty id err=%v", err)
	}
	if _, err := service.RetryFailedIntegrationEvent(context.Background(), managerPrincipal(), IntegrationOutboxLink, "link-1"); err != nil {
		t.Fatal(err)
	}
	if !repository.retried || repository.actor != "ops-1" {
		t.Fatalf("retried=%v actor=%q", repository.retried, repository.actor)
	}
}
