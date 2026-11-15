package contract

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrPaymentTermsInvalid = errors.New("invalid contract payment terms")

// PaymentTermsValidationError 只包含既有付款计划校验的安全原因和真实行字段。
type PaymentTermsValidationError struct {
	Field   string
	Rule    string
	Message string
}

// Error 用途：返回安全付款计划说明，同时保留既有日志前缀。
// 参数：无。
// 返回值：不含被拒绝值的业务错误文本。
func (e *PaymentTermsValidationError) Error() string {
	return ErrPaymentTermsInvalid.Error() + ": " + e.Message
}

// Unwrap 用途：保持付款计划校验错误的原分类。
// 参数：无。
// 返回值：ErrPaymentTermsInvalid。
func (e *PaymentTermsValidationError) Unwrap() error { return ErrPaymentTermsInvalid }

// PublicMessage 用途：明确标识业务分支提供的安全消息。
// 参数：无。
// 返回值：可传输的付款计划拒绝原因。
func (e *PaymentTermsValidationError) PublicMessage() string { return e.Message }

// FieldPath 用途：提供校验失败在原始请求中的字段位置。
// 参数：无。
// 返回值：JSON 字段路径，数组下标按请求原始顺序从零计数。
func (e *PaymentTermsValidationError) FieldPath() string { return e.Field }

const MaxPaymentTerms = 100

// PaymentTerm 是合同付款计划的一期。金额以十进制字符串表达（两位小数），
// 与结算侧 contract.financial_effective.v1 的 payment_terms 契约一致；
// 合计必须等于合同金额，否则合同侧在保存与投递两处都会失败关闭。
type PaymentTerm struct {
	InstallmentNo int    `json:"installment_no"`
	DueDate       string `json:"due_date"`
	Amount        string `json:"amount"`
}

// NormalizePaymentTerms 用途：规范化精确付款计划并保留没有计划的旧合同。
// 参数：terms 为原计划，amountMinor 为合同分金额，currency 为合同币种。
// 返回值：规范化计划；违反既有规则时返回安全字段错误，不改变金额精度。
func NormalizePaymentTerms(terms []PaymentTerm, amountMinor int64, currency string) ([]PaymentTerm, error) {
	if len(terms) == 0 {
		return terms, nil
	}
	if len(terms) > MaxPaymentTerms || amountMinor <= 0 {
		return nil, &PaymentTermsValidationError{"payment_terms", "range", "付款计划最多100期，合同金额必须大于零，请核对后重新保存。"}
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return nil, &PaymentTermsValidationError{"currency", "format", "币种必须是三位字母代码，请重新填写。"}
	}
	for _, char := range currency {
		if char < 'A' || char > 'Z' {
			return nil, &PaymentTermsValidationError{"currency", "format", "币种必须是三位字母代码，请重新填写。"}
		}
	}
	result := make([]PaymentTerm, len(terms))
	seen := map[int]bool{}
	var total int64
	for index, term := range terms {
		if term.InstallmentNo < 1 || term.InstallmentNo > MaxPaymentTerms || seen[term.InstallmentNo] {
			return nil, &PaymentTermsValidationError{fmt.Sprintf("payment_terms[%d].installment_no", index), "range", "分期号必须为1至100且不能重复，请核对后重新保存。"}
		}
		seen[term.InstallmentNo] = true
		date := strings.TrimSpace(term.DueDate)
		due, err := time.Parse("2006-01-02", date)
		if err != nil || due.Year() < 1000 || due.Format("2006-01-02") != date {
			return nil, &PaymentTermsValidationError{fmt.Sprintf("payment_terms[%d].due_date", index), "date", fmt.Sprintf("第%d期日期无效，请重新选择有效日期。", term.InstallmentNo)}
		}
		minor, err := PaymentAmountMinor(term.Amount)
		if err != nil || minor <= 0 || minor > math.MaxInt64-total {
			return nil, &PaymentTermsValidationError{fmt.Sprintf("payment_terms[%d].amount", index), "range", fmt.Sprintf("第%d期金额必须为正数、最多两位小数且不得超出范围，请重新填写。", term.InstallmentNo)}
		}
		total += minor
		result[index] = PaymentTerm{InstallmentNo: term.InstallmentNo, DueDate: date, Amount: fmt.Sprintf("%d.%02d", minor/100, minor%100)}
	}
	if total != amountMinor {
		return nil, &PaymentTermsValidationError{"payment_terms", "total", "付款计划合计必须等于合同金额，请核对各期金额后重新保存。"}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].InstallmentNo < result[j].InstallmentNo })
	return result, nil
}

// PaymentAmountMinor 使用十进制数字而非浮点数，确保
// 财务存储操作前先校验输入精度及溢出。
func PaymentAmountMinor(value string) (int64, error) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, ErrPaymentTermsInvalid
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
		if len(fraction) == 0 || len(fraction) > 2 {
			return 0, ErrPaymentTermsInvalid
		}
	}
	for _, char := range parts[0] + fraction {
		if char < '0' || char > '9' {
			return 0, ErrPaymentTermsInvalid
		}
	}
	for len(fraction) < 2 {
		fraction += "0"
	}
	digits := strings.TrimLeft(parts[0]+fraction, "0")
	if digits == "" {
		return 0, nil
	}
	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, ErrPaymentTermsInvalid
	}
	return minor, nil
}
