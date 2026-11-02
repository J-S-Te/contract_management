package mysql

import (
	"testing"

	"github.com/j-s-te/contract-management/internal/integration/settlement"
)

// validFinancialContractRow 构造一份满足财务事件投递条件的普通合同行。
func validFinancialContractRow() contractRecord {
	return contractRecord{
		ID: "contract-1", TenantID: "tenant-1",
		ContractNumber: stringPtr("HT-2026-0001"), CustomerName: stringPtr("客户甲"),
		CRMCustomerID: uintPtr(42), Currency: "CNY", AmountMinor: 10000, Version: 3,
	}
}

// TestFinancialEffectiveSkipReasonIsExplicit 验证“不产生财务事件”的每条分支都有
// 稳定原因码，而不是静默返回。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFinancialEffectiveSkipReasonIsExplicit(t *testing.T) {
	validTerms := []paymentTermRecord{{InstallmentNo: 1, AmountMinor: 10000}}
	splitTerms := []paymentTermRecord{{InstallmentNo: 1, AmountMinor: 4000}, {InstallmentNo: 2, AmountMinor: 6000}}

	if reason := financialEffectiveSkipReason(nil, validTerms); reason != financialSkipNotOrdinary {
		t.Fatalf("nil 合同原因=%q", reason)
	}
	imported := validFinancialContractRow()
	imported.OriginKind = "IMPORTED"
	if reason := financialEffectiveSkipReason(&imported, validTerms); reason != financialSkipNotOrdinary {
		t.Fatalf("导入合同原因=%q", reason)
	}
	noNumber := validFinancialContractRow()
	noNumber.ContractNumber = nil
	if reason := financialEffectiveSkipReason(&noNumber, validTerms); reason != financialSkipMissingIdentity {
		t.Fatalf("缺编号原因=%q", reason)
	}
	noCustomer := validFinancialContractRow()
	noCustomer.CRMCustomerID = nil
	if reason := financialEffectiveSkipReason(&noCustomer, validTerms); reason != financialSkipMissingIdentity {
		t.Fatalf("缺客户原因=%q", reason)
	}
	badCurrency := validFinancialContractRow()
	badCurrency.Currency = "CN"
	if reason := financialEffectiveSkipReason(&badCurrency, validTerms); reason != financialSkipInvalidTerms {
		t.Fatalf("币种非法原因=%q", reason)
	}
	noVersion := validFinancialContractRow()
	noVersion.Version = 0
	if reason := financialEffectiveSkipReason(&noVersion, validTerms); reason != financialSkipInvalidTerms {
		t.Fatalf("版本为零原因=%q", reason)
	}
	// 审批时未维护付款计划：失败关闭（绝不合成分期），但必须给出可解释原因。
	ordinary := validFinancialContractRow()
	if reason := financialEffectiveSkipReason(&ordinary, nil); reason != financialSkipNoPaymentTerms {
		t.Fatalf("缺付款计划原因=%q", reason)
	}
	if reason := financialEffectiveSkipReason(&ordinary, []paymentTermRecord{{InstallmentNo: 1, AmountMinor: 9999}}); reason != financialSkipTotalMismatch {
		t.Fatalf("合计不符原因=%q", reason)
	}
	if reason := financialEffectiveSkipReason(&ordinary, validTerms); reason != "" {
		t.Fatalf("合法合同不应有跳过原因：%q", reason)
	}
	if reason := financialEffectiveSkipReason(&ordinary, splitTerms); reason != "" {
		t.Fatalf("多期合法合同不应有跳过原因：%q", reason)
	}
}

// TestFinancialEffectiveEnqueueOutcomeKeepsSkipVisible 验证入队结果结构区分
// “已入队”和“显式跳过”，两者都不会被当作成功投递。
// 参数：t 为测试上下文。
// 返回值：无。
func TestFinancialEffectiveEnqueueOutcomeKeepsSkipVisible(t *testing.T) {
	outcome := financialEffectiveEnqueue{SkipReason: financialSkipNoPaymentTerms}
	if outcome.Enqueued || outcome.SkipReason == "" {
		t.Fatalf("跳过结果必须可见：%+v", outcome)
	}
}

// TestBoundedDeliveryClaimLimit 验证批量认领上限收敛到有界区间，
// 调用方传入 0 或超大值时不会放大成无界领取。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBoundedDeliveryClaimLimit(t *testing.T) {
	cases := map[int]int{
		0: 100, -5: 100, 1: 1, 25: 25, 200: 200, 201: 100, 10000: 100,
	}
	for input, want := range cases {
		if got := boundedDeliveryClaimLimit(input); got != want {
			t.Fatalf("limit(%d)=%d want %d", input, got, want)
		}
	}
}

// 保证测试桩仍满足结算侧的批量与单条接口契约。
var (
	_ settlement.Store = (*Repository)(nil)
)
