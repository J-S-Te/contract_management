package contract

import (
	"errors"
	"strings"
	"testing"
)

// TestPaymentFeedbackCarriesExactSafeReason 用途：付款计划的具体拒绝必须带安全原因及真实行字段。
// 参数：t 为测试上下文；日期、金额和合计分别触发现有校验。
// 返回值：无；校验分类、具体原因或字段丢失时失败。
func TestPaymentFeedbackCarriesExactSafeReason(t *testing.T) {
	for _, item := range []struct {
		term            PaymentTerm
		amount          int64
		field, fragment string
	}{
		{PaymentTerm{1, "2027-02-29", "100.00"}, 10000, "payment_terms[0].due_date", "日期"},
		{PaymentTerm{1, "2026-12-01", "secret-value"}, 10000, "payment_terms[0].amount", "金额"},
		{PaymentTerm{1, "2026-12-01", "99.99"}, 10000, "payment_terms", "合计"},
	} {
		_, err := NormalizePaymentTerms([]PaymentTerm{item.term}, item.amount, "CNY")
		feedback, ok := err.(interface {
			PublicMessage() string
			FieldPath() string
		})
		if !errors.Is(err, ErrPaymentTermsInvalid) || !ok {
			t.Fatalf("safe payment error absent: %v", err)
		}
		if feedback.FieldPath() != item.field || !strings.Contains(feedback.PublicMessage(), item.fragment) || strings.Contains(feedback.PublicMessage(), "secret-value") {
			t.Fatalf("wrong safe feedback: %v", err)
		}
	}
}
