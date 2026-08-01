package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDashboardContractJSONUsesBusinessFieldAllowlist(t *testing.T) {
	date := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	encoded, err := json.Marshal(DashboardContract{
		ID: "contract-1", Number: "CON-001", Title: "服务合同", Type: "service",
		ServiceType: "consulting", CustomerCreditLevel: "A", OwnerDisplayName: "章六",
		AmountMinor: 10000, Currency: "CNY", Content: "合同正文", Status: StatusActive,
		StartDate: &date, EndDate: &date, CreatedAt: date, UpdatedAt: date,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	body := string(encoded)
	for _, expected := range []string{"contract_number", "contract_type", "service_type", "customer_credit_level", "owner_display_name", "amount_minor", "currency", "content", "status", "start_date", "end_date", "created_at", "updated_at"} {
		if !strings.Contains(body, `"`+expected+`"`) {
			t.Errorf("dashboard JSON missing %q: %s", expected, body)
		}
	}
	for _, sensitive := range []string{"tenant_id", "owner_user_id", "content_hash", "template_id", "template_values", "version"} {
		if strings.Contains(body, `"`+sensitive+`"`) {
			t.Errorf("dashboard JSON exposes %q: %s", sensitive, body)
		}
	}
}
