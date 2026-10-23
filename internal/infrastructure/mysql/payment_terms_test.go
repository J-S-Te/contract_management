package mysql

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizePaymentTermsRejectsInvalidSchedules(t *testing.T) {
	cases := []struct {
		name     string
		terms    []PaymentTerm
		amount   int64
		wantFail bool
	}{
		{name: "合法两期", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-11-01", Amount: "30000.00"},
			{InstallmentNo: 2, DueDate: "2026-12-01", Amount: "70000.50"},
		}, amount: 10000050},
		{name: "空计划必须失败", terms: nil, amount: 100, wantFail: true},
		{name: "分期号重复", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-11-01", Amount: "50.00"},
			{InstallmentNo: 1, DueDate: "2026-12-01", Amount: "50.00"},
		}, amount: 10000, wantFail: true},
		{name: "分期号非正", terms: []PaymentTerm{
			{InstallmentNo: 0, DueDate: "2026-11-01", Amount: "100.00"},
		}, amount: 10000, wantFail: true},
		{name: "日期格式错误", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026/11/01", Amount: "100.00"},
		}, amount: 10000, wantFail: true},
		{name: "金额三位小数", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-11-01", Amount: "100.001"},
		}, amount: 10000, wantFail: true},
		{name: "金额非正", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-11-01", Amount: "0"},
		}, amount: 0, wantFail: true},
		// 关键失败关闭：合计不等于合同金额时绝不允许落库，否则会向结算发送金额不符的财务事实。
		{name: "合计小于合同金额", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-11-01", Amount: "99.99"},
		}, amount: 10000, wantFail: true},
		{name: "合计大于合同金额", terms: []PaymentTerm{
			{InstallmentNo: 1, DueDate: "2026-11-01", Amount: "100.01"},
		}, amount: 10000, wantFail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			records, total, err := normalizePaymentTerms(tc.terms, tc.amount)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("expected failure, got records=%v total=%d", records, total)
				}
				if !errors.Is(err, ErrPaymentTermsInvalid) {
					t.Fatalf("expected ErrPaymentTermsInvalid, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if total != tc.amount {
				t.Fatalf("total = %d, want %d", total, tc.amount)
			}
			if len(records) != len(tc.terms) {
				t.Fatalf("records = %d, want %d", len(records), len(tc.terms))
			}
		})
	}
}

func TestFormatAmountMinorRoundsToTwoDecimals(t *testing.T) {
	cases := map[int64]string{10000: "100.00", 10000050: "100000.50", 5: "0.05", 123456: "1234.56"}
	for minor, want := range cases {
		if got := formatAmountMinor(minor); got != want {
			t.Fatalf("formatAmountMinor(%d) = %q, want %q", minor, got, want)
		}
	}
}

// 事件载荷必须与结算侧 ContractEvent 的必填字段严格对齐：
// event_type 固定、聚合与合同 id/version 一致、金额为两位小数、付款计划按分期号有序。
func TestNewFinancialEffectivePayloadMatchesSettlementContract(t *testing.T) {
	number := "HT-2026-0001"
	customer := "示例客户"
	crmID := uint64(42)
	row := &contractRecord{
		ID: "01CONTRACT", TenantID: "01TENANT", Version: 3,
		ContractNumber: &number, CustomerName: &customer, CRMCustomerID: &crmID,
		AmountMinor: 10000050, Currency: "cny",
	}
	terms := []paymentTermRecord{
		{InstallmentNo: 1, DueDate: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), AmountMinor: 3000000},
		{InstallmentNo: 2, DueDate: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), AmountMinor: 7000050},
	}
	raw, err := newFinancialEffectivePayload("01EVENT", row, terms, time.Date(2026, 10, 13, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var payload financialEffectivePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.EventType != "contract.financial_effective.v1" {
		t.Fatalf("event_type = %q", payload.EventType)
	}
	if payload.EventID != "01EVENT" || payload.TenantID != "01TENANT" {
		t.Fatalf("event identity mismatch: %+v", payload)
	}
	if payload.Aggregate.ID != payload.Contract.ID || payload.Aggregate.Version != payload.Contract.Version {
		t.Fatalf("aggregate must mirror contract: %+v", payload.Aggregate)
	}
	if payload.Contract.No != number || payload.Contract.CustomerID != "42" || payload.Contract.CustomerName != customer {
		t.Fatalf("contract identity mismatch: %+v", payload.Contract)
	}
	if payload.Contract.Currency != "CNY" {
		t.Fatalf("currency must be upper-case three-letter, got %q", payload.Contract.Currency)
	}
	if payload.Contract.Amount != "100000.50" {
		t.Fatalf("amount = %q", payload.Contract.Amount)
	}
	if len(payload.Contract.PaymentTerms) != 2 ||
		payload.Contract.PaymentTerms[0].DueDate != "2026-11-01" ||
		payload.Contract.PaymentTerms[1].Amount != "70000.50" {
		t.Fatalf("payment terms mismatch: %+v", payload.Contract.PaymentTerms)
	}
	// 项目字段非必填，且不应编造。
	if payload.Contract.ProjectID != "" || payload.Contract.ProjectName != "" {
		t.Fatalf("project fields must stay empty: %+v", payload.Contract)
	}
	if !strings.Contains(string(raw), "\"occurred_at\"") {
		t.Fatalf("occurred_at missing: %s", raw)
	}
}

func TestFinancialPaymentDueDateRemainsCalendarDateInConfiguredDBLocation(t *testing.T) {
	date := time.Date(2026, 12, 1, 0, 0, 0, 0, time.FixedZone("Shanghai", 8*60*60))
	raw, err := newFinancialEffectivePayload("event", &contractRecord{}, []paymentTermRecord{{InstallmentNo: 1, DueDate: date, AmountMinor: 100}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var payload financialEffectivePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Contract.PaymentTerms[0].DueDate != "2026-12-01" {
		t.Fatalf("calendar date moved across timezone: %s", payload.Contract.PaymentTerms[0].DueDate)
	}
}
