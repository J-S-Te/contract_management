package contract

import (
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxPricingMinor int64 = 9007199254740991

var ErrPricing = errors.New("合同定价不完整或标准价不匹配")
var quantityPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.[0-9]{1,4})?$`)

type StandardPrice struct {
	ID              string     `json:"id"`
	TenantID        string     `json:"-"`
	Name            string     `json:"name"`
	CategoryID      string     `json:"category_id"`
	CategoryVersion uint64     `json:"category_version,omitempty"`
	ServiceType     string     `json:"service_type"`
	OrganizationID  string     `json:"organization_id,omitempty"`
	Unit            string     `json:"unit"`
	UnitPriceMinor  int64      `json:"unit_price_minor"`
	Currency        string     `json:"currency"`
	TaxBasis        string     `json:"tax_basis"`
	TaxRateBPS      int        `json:"tax_rate_bps"`
	EffectiveFrom   string     `json:"effective_from"`
	EffectiveTo     string     `json:"effective_to,omitempty"`
	Status          string     `json:"status"`
	Version         uint64     `json:"version"`
	CreatedBy       string     `json:"created_by"`
	UpdatedBy       string     `json:"updated_by"`
	PublishedBy     string     `json:"published_by,omitempty"`
	PublishedAt     *time.Time `json:"published_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type PricingLine struct {
	ServiceIndex   int    `json:"service_index"`
	ServiceID      string `json:"service_id"`
	LineID         string `json:"line_id"`
	PriceID        string `json:"price_id"`
	PriceVersion   uint64 `json:"price_version"`
	Quantity       string `json:"quantity"`
	Unit           string `json:"unit"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	AmountMinor    int64  `json:"amount_minor"`
	StandardMinor  int64  `json:"standard_minor"`
}

type Pricing struct {
	BasisDate  string        `json:"basis_date"`
	TaxBasis   string        `json:"tax_basis"`
	TaxRateBPS int           `json:"tax_rate_bps"`
	Currency   string        `json:"currency"`
	Lines      []PricingLine `json:"lines"`
	Reselect   bool          `json:"reselect,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	FrozenAt   time.Time     `json:"frozen_at"`
}

// DiscountBucket 将成交额与完整标准额进行精确分桶。
// 参数：amount为成交最小币种单位，standard为冻结标准最小币种单位。
// 返回值：标准桶码；非法金额返回空。
func DiscountBucket(amount, standard int64) string {
	if amount < 0 || standard <= 0 || amount > MaxPricingMinor || standard > MaxPricingMinor {
		return ""
	}
	a := new(big.Int).Mul(big.NewInt(amount), big.NewInt(100))
	if a.Cmp(new(big.Int).Mul(big.NewInt(standard), big.NewInt(80))) < 0 {
		return "LT_80"
	}
	if a.Cmp(new(big.Int).Mul(big.NewInt(standard), big.NewInt(90))) < 0 {
		return "FROM_80_TO_90"
	}
	if amount < standard {
		return "FROM_90_TO_100"
	}
	if amount == standard {
		return "AT_100"
	}
	return "GT_100"
}

// QuantityAmount 按四位定点数量乘单价并四舍五入到最小币种单位。
// 参数：quantity为十进制正数量，unitPrice为正整数单价。
// 返回值：舍入后的行标准额；精度、边界或溢出错误返回ErrPricing。
func QuantityAmount(quantity string, unitPrice int64) (int64, error) {
	if !quantityPattern.MatchString(quantity) || unitPrice <= 0 || unitPrice > MaxPricingMinor {
		return 0, ErrPricing
	}
	parts := strings.Split(quantity, ".")
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	scaled, err := strconv.ParseInt(parts[0]+fraction+strings.Repeat("0", 4-len(fraction)), 10, 64)
	if err != nil || scaled <= 0 {
		return 0, ErrPricing
	}
	result := new(big.Int).Mul(big.NewInt(scaled), big.NewInt(unitPrice))
	result.Add(result, big.NewInt(5000))
	result.Quo(result, big.NewInt(10000))
	if !result.IsInt64() || result.Int64() <= 0 || result.Int64() > MaxPricingMinor {
		return 0, ErrPricing
	}
	return result.Int64(), nil
}

// PricingTotal 校验冻结定价完整覆盖、币税一致和成交金额守恒。
// 参数：c为含服务项、金额与冻结定价的权威合同。
// 返回值：完整标准总额；历史无基准或任何不一致返回ErrPricing。
func PricingTotal(c Contract) (int64, error) {
	p := c.Pricing
	if p == nil || len(p.Lines) == 0 || len(p.Lines) != len(c.ServiceItems) || p.Currency != c.Currency || p.FrozenAt.IsZero() || p.TaxRateBPS < 0 || p.TaxRateBPS > 10000 || (p.TaxBasis != "INCLUSIVE" && p.TaxBasis != "EXCLUSIVE") {
		return 0, ErrPricing
	}
	seen := map[string]bool{}
	var actual, standard int64
	for _, l := range p.Lines {
		if l.ServiceIndex < 0 || l.ServiceIndex >= len(c.ServiceItems) || l.ServiceID == "" || l.ServiceID != c.ServiceItems[l.ServiceIndex].SourceID || seen[l.ServiceID] || l.LineID == "" || l.PriceID == "" || l.PriceVersion == 0 || l.AmountMinor < 0 || l.AmountMinor > MaxPricingMinor {
			return 0, ErrPricing
		}
		expected, err := QuantityAmount(l.Quantity, l.UnitPriceMinor)
		if err != nil || expected != l.StandardMinor || actual > MaxPricingMinor-l.AmountMinor || standard > MaxPricingMinor-l.StandardMinor {
			return 0, ErrPricing
		}
		seen[l.ServiceID] = true
		actual += l.AmountMinor
		standard += l.StandardMinor
	}
	if actual != c.AmountMinor {
		return 0, ErrPricing
	}
	return standard, nil
}

// ValidateStandardPrice 校验标准价草稿的业务字段。
// 参数：p为待保存的目录记录。
// 返回值：字段合法返回空，否则返回ErrPricing。
func ValidateStandardPrice(p StandardPrice) error {
	from, err := time.Parse("2006-01-02", p.EffectiveFrom)
	if err != nil {
		return ErrPricing
	}
	if p.EffectiveTo != "" {
		to, e := time.Parse("2006-01-02", p.EffectiveTo)
		if e != nil || to.Before(from) {
			return ErrPricing
		}
	}
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 200 || p.CategoryID == "" || len(p.CategoryID) > 128 || p.ServiceType == "" || len(p.ServiceType) > 100 || p.Unit == "" || len(p.Unit) > 40 || len(p.OrganizationID) > 128 || !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(p.Currency) || p.UnitPriceMinor <= 0 || p.UnitPriceMinor > MaxPricingMinor || p.TaxRateBPS < 0 || p.TaxRateBPS > 10000 || p.TaxBasis != "INCLUSIVE" && p.TaxBasis != "EXCLUSIVE" {
		return ErrPricing
	}
	return nil
}
