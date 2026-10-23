package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExternalMetadataErrorsAreActionableAndDoNotEchoInput(t *testing.T) {
	for _, test := range []struct {
		name, payload, reason, message string
	}{
		{"unknown field", `{"private-contract-value":"secret-document-content"}`, "unknown_field", "前后端版本一致"},
		{"customer ID", `{"crm_customer_id":"secret-customer-value"}`, "invalid_type", "CRM 客户格式不正确"},
		{"nested service field", `{"service_items":[{"systems":"secret-system-value"}]}`, "invalid_type", "服务项格式不正确"},
		{"date", `{"start_date":"secret-date-value"}`, "invalid_date", "重新选择日期"},
		{"broken JSON", `{"title":`, "invalid_json", "重新填写"},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := json.NewDecoder(strings.NewReader(test.payload))
			decoder.DisallowUnknownFields()
			var metadata externalContractMetadata
			err := decoder.Decode(&metadata)
			if err == nil {
				t.Fatal("invalid payload unexpectedly accepted")
			}
			message, details := externalMetadataDecodeError(err)
			if details["reason"] != test.reason || !strings.Contains(message, test.message) {
				t.Fatalf("unexpected diagnosis: %s %+v", message, details)
			}
			raw, marshalErr := json.Marshal(details)
			if marshalErr != nil || strings.Contains(message+string(raw), "secret-") || strings.Contains(string(raw), "private-contract-value") {
				t.Fatal("diagnosis contains raw contract data")
			}
		})
	}
}
