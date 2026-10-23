package contract

import (
	"errors"
	"reflect"
	"testing"
)

func TestPaymentPlanNormalizesExactDecimalsWithoutInventingLegacyTerms(t *testing.T) {
	for _, terms := range [][]PaymentTerm{nil, {}} {
		got, err := NormalizePaymentTerms(terms, 0, "CNY")
		if err != nil || len(got) != 0 {
			t.Fatalf("legacy terms were fabricated: %v %v", got, err)
		}
	}
	terms := []PaymentTerm{{InstallmentNo: 2, DueDate: "2027-01-01", Amount: " 0.2 "}, {InstallmentNo: 1, DueDate: "2026-12-01", Amount: "0.10"}}
	got, err := NormalizePaymentTerms(terms, 30, " cny ")
	if err != nil || got[0].InstallmentNo != 1 || got[1].Amount != "0.20" || terms[0].Amount != " 0.2 " {
		t.Fatalf("normalization: %v %v", got, err)
	}
}

func TestPaymentPlanRejectsInvalidTotalsDatesNumbersCurrencyAndOverflow(t *testing.T) {
	valid := []PaymentTerm{{InstallmentNo: 1, DueDate: "2028-02-29", Amount: "100.00"}}
	for _, tc := range []struct {
		name     string
		terms    []PaymentTerm
		amount   int64
		currency string
	}{
		{"sum mismatch", valid, 9999, "CNY"},
		{"currency", valid, 10000, "C1Y"},
		{"zero contract", valid, 0, "CNY"},
		{"fraction", []PaymentTerm{{1, "2026-12-01", "100.001"}}, 10000, "CNY"},
		{"exponent", []PaymentTerm{{1, "2026-12-01", "1e2"}}, 10000, "CNY"},
		{"negative", []PaymentTerm{{1, "2026-12-01", "-100"}}, 10000, "CNY"},
		{"zero term", []PaymentTerm{{1, "2026-12-01", "0"}}, 10000, "CNY"},
		{"date", []PaymentTerm{{1, "2027-02-29", "100"}}, 10000, "CNY"},
		{"db date range", []PaymentTerm{{1, "0001-01-01", "100"}}, 10000, "CNY"},
		{"missing number", []PaymentTerm{{0, "2026-12-01", "100"}}, 10000, "CNY"},
		{"duplicate", []PaymentTerm{{1, "2026-12-01", "50"}, {1, "2027-01-01", "50"}}, 10000, "CNY"},
		{"overflow", []PaymentTerm{{1, "2026-12-01", "92233720368547758.08"}}, 10000, "CNY"},
		{"sum overflow", []PaymentTerm{{1, "2026-12-01", "92233720368547758.07"}, {2, "2027-01-01", "1"}}, 10000, "CNY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizePaymentTerms(tc.terms, tc.amount, tc.currency); !errors.Is(err, ErrPaymentTermsInvalid) {
				t.Fatalf("accepted invalid plan: %v", err)
			}
		})
	}
	if _, err := NormalizePaymentTerms(make([]PaymentTerm, 101), 10000, "CNY"); !errors.Is(err, ErrPaymentTermsInvalid) {
		t.Fatal("unbounded schedule accepted")
	}
	if got, err := NormalizePaymentTerms(valid, 10000, "CNY"); err != nil || !reflect.DeepEqual(got, valid) {
		t.Fatal(got, err)
	}
}
