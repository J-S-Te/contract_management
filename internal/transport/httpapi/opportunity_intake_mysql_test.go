package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/infrastructure/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestCMROpportunityIntakeRoutePersistsAcceptedIntakeOnMySQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CONTRACT_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("CONTRACT_TEST_MYSQL_DSN is required")
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	service := &application.Service{Repo: mysql.NewRepository(db)}
	router := NewRouterWithAllIntegrations(service, intakeRouteIdentity{}, nil, nil, nil, &CRMSignedCountIntegrationOptions{
		OpportunityIntakeEnabled: true, OpportunityIntakeRequireBearer: true, OpportunityIntakeBearerVerifier: stubVerifier{tenantID: "tenant-contract-intake-e2e"},
	}, nil, nil)
	eventID := fmt.Sprintf("evt-intake-%d", time.Now().UTC().UnixNano())
	body := fmt.Sprintf(`{"event_id":%q,"opportunity_id":7,"event_version":1,"opportunity_no":"SJ-7","customer_id":9,"contract_ref":"HT-7","expected_amount":"100.00","occurred_at":"2026-10-18T12:00:00Z"}`, eventID)
	request := httptest.NewRequest(http.MethodPost, "/internal/opportunity-signed-events", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer machine-token")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"status":"ACCEPTED"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var count int64
	if err := db.Table("con_opportunity_intake").Where("tenant_id=? AND event_id=?", "tenant-contract-intake-e2e", eventID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persisted intakes=%d, want 1", count)
	}
}
