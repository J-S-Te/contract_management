package mysql

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

func TestProjectActivationPayloadIncludesAuthoritativeContractNumber(t *testing.T) {
	number := "HT-2026-0001"
	customer := "示例客户"
	payload := newProjectActivationPayload(contractRecord{
		ID: "contract-1", ContractNumber: &number, Version: 3, Title: "外部合同", CustomerName: &customer,
	}, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), false, []contract.ProjectServiceItem{{SourceID: "service-1"}})

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.ContractNumber != number || !strings.Contains(string(encoded), `"contract_number":"HT-2026-0001"`) {
		t.Fatalf("payload=%+v json=%s", payload, encoded)
	}
}

func TestProjectActivationPayloadKeepsLegacyEmptyContractNumber(t *testing.T) {
	payload := newProjectActivationPayload(contractRecord{ID: "contract-1", Version: 1}, time.Now(), false, nil)
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"contract_number":""`) {
		t.Fatalf("legacy payload must keep an explicit empty contract_number for receiver fallback: %s", encoded)
	}
}
