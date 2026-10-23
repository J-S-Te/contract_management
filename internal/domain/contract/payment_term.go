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

const MaxPaymentTerms = 100

// PaymentTerm 是合同付款计划的一期。金额以十进制字符串表达（两位小数），
// 与结算侧 contract.financial_effective.v1 的 payment_terms 契约一致；
// 合计必须等于合同金额，否则合同侧在保存与投递两处都会失败关闭。
type PaymentTerm struct {
	InstallmentNo int    `json:"installment_no"`
	DueDate       string `json:"due_date"`
	Amount        string `json:"amount"`
}

// NormalizePaymentTerms 保留没有计划的旧合同。
// 显式提供的计划必须完整；禁止凭空创建分期，
// 或通过货币舍入使其与合同平衡。
func NormalizePaymentTerms(terms []PaymentTerm, amountMinor int64, currency string) ([]PaymentTerm, error) {
	if len(terms) == 0 {
		return terms, nil
	}
	if len(terms) > MaxPaymentTerms || amountMinor <= 0 {
		return nil, fmt.Errorf("%w: 付款计划最多100期，合同金额必须大于零", ErrPaymentTermsInvalid)
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if len(currency) != 3 {
		return nil, fmt.Errorf("%w: 币种必须是三位字母代码", ErrPaymentTermsInvalid)
	}
	for _, char := range currency {
		if char < 'A' || char > 'Z' {
			return nil, fmt.Errorf("%w: 币种必须是三位字母代码", ErrPaymentTermsInvalid)
		}
	}
	result := make([]PaymentTerm, len(terms))
	seen := map[int]bool{}
	var total int64
	for index, term := range terms {
		if term.InstallmentNo < 1 || term.InstallmentNo > MaxPaymentTerms || seen[term.InstallmentNo] {
			return nil, fmt.Errorf("%w: 分期号必须为1至100且不能重复", ErrPaymentTermsInvalid)
		}
		seen[term.InstallmentNo] = true
		date := strings.TrimSpace(term.DueDate)
		due, err := time.Parse("2006-01-02", date)
		if err != nil || due.Year() < 1000 || due.Format("2006-01-02") != date {
			return nil, fmt.Errorf("%w: 第%d期日期必须是有效的YYYY-MM-DD", ErrPaymentTermsInvalid, term.InstallmentNo)
		}
		minor, err := PaymentAmountMinor(term.Amount)
		if err != nil || minor <= 0 || minor > math.MaxInt64-total {
			return nil, fmt.Errorf("%w: 第%d期金额必须为正数、最多两位小数且不得超出范围", ErrPaymentTermsInvalid, term.InstallmentNo)
		}
		total += minor
		result[index] = PaymentTerm{InstallmentNo: term.InstallmentNo, DueDate: date, Amount: fmt.Sprintf("%d.%02d", minor/100, minor%100)}
	}
	if total != amountMinor {
		return nil, fmt.Errorf("%w: 付款计划合计必须等于合同金额", ErrPaymentTermsInvalid)
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
