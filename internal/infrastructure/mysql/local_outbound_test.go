package mysql

import (
	"errors"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// TestLocalOutboundHasExplicitFrozenDisposition 验证本地引用保持不适用且不伪装缺CRM身份。
// 参数：t为测试上下文。
// 返回值：无。
func TestLocalOutboundHasExplicitFrozenDisposition(t *testing.T) {
	row := contractRecord{CounterpartySource: contract.CounterpartyLocal, IntegrationPolicy: contract.IntegrationNotApplicable}
	skip, err := localOutboundNotApplicable(row)
	if !skip || err != nil {
		t.Fatalf("local disposition %v %v", skip, err)
	}
	if reason := financialEffectiveSkipReason(&row, nil); reason != "local_source_not_applicable" {
		t.Fatalf("local misclassified: %s", reason)
	}
	row.IntegrationPolicy = ""
	if _, err := localOutboundNotApplicable(row); !errors.Is(err, application.ErrContractSourceConflict) {
		t.Fatalf("damaged local policy accepted: %v", err)
	}
	legacy := contractRecord{}
	if skip, err := localOutboundNotApplicable(legacy); skip || err != nil {
		t.Fatal("legacy behavior changed")
	}
	row.IntegrationPolicy = contract.IntegrationNotApplicable
	if err := validateOpportunityLinkTarget(row, opportunityIntakeRecord{CustomerID: 42}); !errors.Is(err, application.ErrOpportunityLinkRejected) {
		t.Fatalf("LOCAL CRM takeover accepted: %v", err)
	}
}
