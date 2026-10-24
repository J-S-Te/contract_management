package httpapi

import (
	"net/http/httptest"
	"testing"

	core "github.com/J-S-Te/license-core"
	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

func TestContractHistoryLicenseRetainsHandoverReadsAndAuditRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		method, path, request string
		operation             core.Operation
		want                  int
	}{
		{"GET", "/internal/v1/handover/responsibilities", "/internal/v1/handover/responsibilities", core.READ_HISTORY, 204},
		{"GET", "/internal/v1/handover/transfers/:itemID", "/internal/v1/handover/transfers/old-item", core.READ_HISTORY, 204},
		{"GET", "/api/v1/audit-outbox/failed", "/api/v1/audit-outbox/failed", core.ESSENTIAL_SERVICE, 204},
		{"POST", "/api/v1/audit-outbox/failed/:id/retry", "/api/v1/audit-outbox/failed/old-event/retry", core.ESSENTIAL_SERVICE, 204},
		{"POST", "/internal/v1/handover/freeze", "/internal/v1/handover/freeze", core.MUTATE_BUSINESS, 403},
		{"POST", "/internal/v1/handover/transfers", "/internal/v1/handover/transfers", core.MUTATE_BUSINESS, 403},
		{"POST", "/internal/v1/handover/complete", "/internal/v1/handover/complete", core.MUTATE_BUSINESS, 403},
		{"POST", "/api/v1/approvals/:approvalID/recover", "/api/v1/approvals/old/recover", core.MUTATE_BUSINESS, 403},
		{"POST", "/internal/opportunity-signed-events", "/internal/opportunity-signed-events", core.MUTATE_BUSINESS, 403},
		{"GET", "/internal/v1/handover/new-effect", "/internal/v1/handover/new-effect", core.MUTATE_BUSINESS, 403},
		{"POST", "/internal/v1/handover/responsibilities", "/internal/v1/handover/responsibilities", core.MUTATE_BUSINESS, 403},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			if got := contractLicenseOperation(tc.method, tc.path); got != tc.operation {
				t.Errorf("classification=%s want=%s", got, tc.operation)
			}
			h := &Handler{service: &application.Service{LicenseGate: historyOnlyLicense{}}}
			r := gin.New()
			r.Use(h.licenseOperation())
			r.Handle(tc.method, tc.path, func(c *gin.Context) { c.Status(204) })
			response := httptest.NewRecorder()
			r.ServeHTTP(response, httptest.NewRequest(tc.method, tc.request, nil))
			if response.Code != tc.want {
				t.Errorf("history-only HTTP status=%d want=%d", response.Code, tc.want)
			}
		})
	}
}
