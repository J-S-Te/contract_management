package mysql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestLocalSourceRoundTripWithoutWIPChanges 验证既有来源helper完成本地字段原子写回与恢复。
// 参数：t为测试上下文。
// 返回值：无；不用修改CreateContract或contractFromRecord的既有接线。
func TestLocalSourceRoundTripWithoutWIPChanges(t *testing.T) {
	source := contract.Contract{CounterpartySource: contract.CounterpartyLocal, LocalCounterpartyID: "01J00000000000000000000001", CounterpartyVersion: 2, CategorySource: contract.CategoryLocal, IntegrationPolicy: contract.IntegrationNotApplicable}
	var record contractRecord
	if err := applyContractSource(&record, source); err != nil {
		t.Fatal(err)
	}
	got := contractFromRecord(record)
	if got.CounterpartySource != contract.CounterpartyLocal || got.LocalCounterpartyID != source.LocalCounterpartyID || got.CounterpartyVersion != 2 || got.IntegrationPolicy != contract.IntegrationNotApplicable {
		t.Fatalf("local reference dropped: %+v", got)
	}
	old := contractFromRecord(contractRecord{CRMCustomerID: uintPtr(42), OriginKind: "NORMAL"})
	data, _ := json.Marshal(old)
	for _, key := range []string{"counterparty_source", "local_counterparty_id", "counterparty_version", "category_source", "integration_policy"} {
		if strings.Contains(string(data), `"`+key+`"`) {
			t.Fatalf("legacy JSON changed: %s", data)
		}
	}
}
