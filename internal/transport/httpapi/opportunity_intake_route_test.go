package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
)

type intakeRouteIdentity struct{}

func (intakeRouteIdentity) Authenticate(context.Context, *http.Request) (application.Principal, error) {
	return application.Principal{}, nil
}

func TestCMROpportunityIntakeRouteIsRegisteredAtWorkerPath(t *testing.T) {
	router := NewRouterWithAllIntegrations(nil, intakeRouteIdentity{}, nil, nil, nil, &CRMSignedCountIntegrationOptions{
		OpportunityIntakeEnabled: true, OpportunityIntakeRequireBearer: true,
	}, nil, nil)
	found := false
	for _, route := range router.Routes() {
		if route.Method == http.MethodPost && route.Path == "/internal/opportunity-signed-events" {
			found = true
		}
	}
	if !found {
		t.Fatalf("opportunity intake route missing: %+v", router.Routes())
	}
}

type intakeOnlyRepository struct {
	application.Repository
	accepted *application.OpportunityIntake
}

func (repository *intakeOnlyRepository) AcceptOpportunityIntake(_ context.Context, in application.OpportunityIntake) (application.OpportunityIntake, error) {
	repository.accepted = &in
	in.Status = application.OpportunityIntakeAccepted
	return in, nil
}
func (*intakeOnlyRepository) ListOpportunityIntakes(context.Context, string, string, string, int) (application.OpportunityIntakePage, error) {
	return application.OpportunityIntakePage{}, nil
}
func (*intakeOnlyRepository) GetOpportunityIntake(context.Context, string, string) (application.OpportunityIntake, error) {
	return application.OpportunityIntake{}, nil
}
func (*intakeOnlyRepository) ReviewOpportunityIntake(context.Context, string, string, string, application.OpportunityIntakeReview, string, string, string) (application.OpportunityIntake, error) {
	return application.OpportunityIntake{}, nil
}

func TestCMROpportunityIntakeRouteAcceptsMachineEnvelope(t *testing.T) {
	repo := &intakeOnlyRepository{}
	service := &application.Service{Repo: repo}
	router := NewRouterWithAllIntegrations(service, intakeRouteIdentity{}, nil, nil, nil, &CRMSignedCountIntegrationOptions{
		OpportunityIntakeEnabled: true, OpportunityIntakeRequireBearer: true, OpportunityIntakeBearerVerifier: stubVerifier{tenantID: "tenant-1"},
	}, nil, nil)
	body := `{"event_id":"evt-1","opportunity_id":7,"event_version":1,"opportunity_no":"SJ-7","customer_id":9,"contract_ref":"HT-7","expected_amount":"100.00","occurred_at":"2026-10-18T12:00:00Z"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/opportunity-signed-events", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"status":"ACCEPTED"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repo.accepted == nil || repo.accepted.TenantID != "tenant-1" || repo.accepted.EventID != "evt-1" {
		t.Fatalf("accepted=%+v", repo.accepted)
	}
}
