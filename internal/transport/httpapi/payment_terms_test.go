package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func TestPaymentTermsAcceptedByExistingContractDTOs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	payload := `{"payment_terms":[{"installment_no":1,"due_date":"2026-12-01","amount":"100.00"}]}`
	for _, external := range []bool{false, true} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/v1/contracts", strings.NewReader(payload))
		if external {
			var body externalContractMetadata
			if !decode(c, &body) {
				t.Fatalf("external contract metadata rejected payment terms: %s", w.Body.String())
			}
		} else {
			var body createContractRequest
			if !decode(c, &body) {
				t.Fatalf("template/draft metadata rejected payment terms: %s", w.Body.String())
			}
			encoded, err := json.Marshal(body)
			if err != nil || !strings.Contains(string(encoded), `"payment_terms"`) {
				t.Fatal("payment terms lost during DTO decode")
			}
		}
	}
}

type completedPaymentTermsRepository struct{ application.Repository }

func (completedPaymentTermsRepository) ListContracts(context.Context, string, string, string, string, int) ([]contract.Contract, error) {
	return []contract.Contract{{ID: "completed", TenantID: "tenant", Status: contract.StatusCompleted, PaymentTerms: []contract.PaymentTerm{{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "100.00"}}}}, nil
}

func TestCompletedContractSummaryReturnsStoredPaymentTerms(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/internal/v1/settlement/completed-contracts", nil)
	c.Set(principalKey, application.Principal{TenantID: "tenant", Permissions: map[string]bool{"contract.read": true}, PermissionScopes: map[string]contract.ScopeFilter{"contract.read": {AllowAll: true}}})
	h := &Handler{service: &application.Service{Repo: completedPaymentTermsRepository{}}}
	h.listSettlementCompletedContracts(c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"payment_terms":[{"installment_no":1,"due_date":"2026-12-01","amount":"100.00"}]`) {
		t.Fatalf("completed summary lost authoritative schedule: %d %s", w.Code, w.Body.String())
	}
}
