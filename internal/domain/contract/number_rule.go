package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	// NumberYearSegmentNone 表示单号不含年份段。
	NumberYearSegmentNone = "NONE"
	// NumberYearSegmentYY 表示两位年份段（如 26）。
	NumberYearSegmentYY = "YY"
	// NumberYearSegmentYYYY 表示四位年份段（如 2026）。
	NumberYearSegmentYYYY = "YYYY"

	// ContractNumberMaxLength 与 con_contract.contract_number 的列宽保持一致。
	ContractNumberMaxLength = 64
	// ContractNumberAffixMaxLength 是前缀、后缀各自允许的最大长度。
	ContractNumberAffixMaxLength = 32
	// ContractNumberSeqWidthMin 与 ContractNumberSeqWidthMax 限定流水位宽。
	ContractNumberSeqWidthMin = 1
	ContractNumberSeqWidthMax = 9
)

var (
	// ErrInvalidNumberRule 表示系统级编号规则不满足格式约束，属于可修正配置。
	ErrInvalidNumberRule = errors.New("invalid contract number rule")
	// ErrNumberExhausted 表示流水超出位宽上限，或顺延取号预算耗尽；
	// 这是确定性业务错误，不能退化为可无限重试的执行故障。
	ErrNumberExhausted = errors.New("contract number sequence exhausted")
	// ErrNumberConflict 表示审批取号后写号时命中 con_contract 的
	// uk_con_contract_number(tenant_id, contract_number) 唯一键：该租户已经存在同号合同，
	// 靠重试永远无法成功，必须人工受控更正数据。与 ErrNumberExhausted 一样属于
	// 确定性业务冲突，工作流必须立即失败而不是无限重试。
	ErrNumberConflict = errors.New("contract number conflicts with an existing contract")
)

// ContractNumberRule 是系统级合同编号规则：全系统共用一个前缀配置，
// 流水是全系统一个年度计数器，按 (年份, 规则指纹) 分桶。UpdatedBy/UpdatedAt 仅用于展示，
// 不参与规则指纹，改审计字段不会换桶。
type ContractNumberRule struct {
	Prefix      string    `json:"prefix"`
	YearSegment string    `json:"year_segment"`
	SeqWidth    int       `json:"seq_width"`
	Suffix      string    `json:"suffix"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// DefaultContractNumberRule 提供与迁移 000037 种子行一致的展示默认规则。
// 参数：无；本规则只供缺失规则行时展示，不参与旧部署取号回退。
// 返回值：默认合同编号规则。
func DefaultContractNumberRule() ContractNumberRule {
	return ContractNumberRule{Prefix: "HT-", YearSegment: NumberYearSegmentYYYY, SeqWidth: 4}
}

// NormalizeContractNumberRule 归一化并校验规则，保证持久化后一定能安全拼装单号。
// 参数：rule 为外部输入或数据库读出的规则。
// 返回值：归一化规则，或包装 ErrInvalidNumberRule 的中文原因。
func NormalizeContractNumberRule(rule ContractNumberRule) (ContractNumberRule, error) {
	rule.Prefix = strings.TrimSpace(rule.Prefix)
	rule.Suffix = strings.TrimSpace(rule.Suffix)
	rule.YearSegment = strings.ToUpper(strings.TrimSpace(rule.YearSegment))
	if rule.YearSegment == "" {
		rule.YearSegment = NumberYearSegmentYYYY
	}
	if err := validateContractNumberAffix("前缀", rule.Prefix); err != nil {
		return ContractNumberRule{}, err
	}
	if err := validateContractNumberAffix("后缀", rule.Suffix); err != nil {
		return ContractNumberRule{}, err
	}
	switch rule.YearSegment {
	case NumberYearSegmentNone, NumberYearSegmentYY, NumberYearSegmentYYYY:
	default:
		return ContractNumberRule{}, fmt.Errorf("%w: 年份段只能是 NONE、YY 或 YYYY", ErrInvalidNumberRule)
	}
	if rule.SeqWidth < ContractNumberSeqWidthMin || rule.SeqWidth > ContractNumberSeqWidthMax {
		return ContractNumberRule{}, fmt.Errorf("%w: 流水位宽必须是 %d 到 %d 之间的整数", ErrInvalidNumberRule,
			ContractNumberSeqWidthMin, ContractNumberSeqWidthMax)
	}
	// 按位宽上限估算最长单号，避免保存出必然超出数据库列宽的规则。
	longest := len([]rune(rule.Prefix)) + contractNumberYearSegmentLength(rule.YearSegment) +
		rule.SeqWidth + len([]rune(rule.Suffix))
	if longest > ContractNumberMaxLength {
		return ContractNumberRule{}, fmt.Errorf("%w: 规则最长可生成 %d 个字符的单号，超过 %d 个字符上限，请缩短前缀、后缀或流水位宽",
			ErrInvalidNumberRule, longest, ContractNumberMaxLength)
	}
	return rule, nil
}

// ContractNumberAffixAllowedChars 是前缀/后缀允许的字符集说明，与
// ContractNumberAffixAllowed 一一对应，用于错误消息与接口文档保持一致。
const ContractNumberAffixAllowedChars = "只能包含字母、数字和 - _ . /"

// validateContractNumberAffix 校验前缀/后缀是否落在下游系统可接受的字符白名单内。
//
// 白名单与 CRM 征信投递的编号校验（^[A-Za-z0-9._/-]+$）保持一致：合同编号会随结算
// 回款征信投递给 CRM，一旦管理员把中文、空格、括号、#、{} 配进前缀，下游会直接 400，
// 而且是跨系统故障，只能靠改规则回滚。因此这里在保存规则时就拒绝，不等到投递失败。
// 大括号同时被白名单排除，旧模板占位符 {YYYYMMDD}/{ID8} 也就不会再混进系统编号。
func validateContractNumberAffix(label, value string) error {
	if len([]rune(value)) > ContractNumberAffixMaxLength {
		return fmt.Errorf("%w: %s不能超过 %d 个字符", ErrInvalidNumberRule, label, ContractNumberAffixMaxLength)
	}
	for _, char := range value {
		if !ContractNumberAffixAllowed(char) {
			return fmt.Errorf("%w: %s%s（当前含非法字符 %q）", ErrInvalidNumberRule, label, ContractNumberAffixAllowedChars, char)
		}
	}
	return nil
}

// ContractNumberAffixAllowed 判断单个字符是否属于编号白名单：A-Z a-z 0-9 - _ . /
// 参数：char 为待判断字符。
// 返回值：允许时为 true。
// 该白名单同时被合同编号规则前缀/后缀和模板 number_format 复用，避免两处字符集漂移。
func ContractNumberAffixAllowed(char rune) bool {
	switch {
	case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		return true
	case char == '-', char == '_', char == '.', char == '/':
		return true
	default:
		return false
	}
}

// ContractNumberRuleKeyMaxLength 与 con_contract_number_sequence.rule_key 的列宽一致。
const ContractNumberRuleKeyMaxLength = 64

// ContractNumberRuleKey 计算规则的稳定指纹，用作年度流水的分桶键。
//
// 参数：rule 为外部输入或数据库读出的规则，内部先归一化再取指纹，
// 因此只差首尾空白、大小写年份段的等价规则得到同一个 key。
// 返回值：sha256(prefix \x00 year_segment \x00 seq_width \x00 suffix) 的小写十六进制，
// 固定 64 个字符；不含随机量与时间戳，同一规则任何时候都得到同一 key，
// 任一参与拼装单号的字段变化都会得到不同 key。规则非法时返回 ErrInvalidNumberRule 包装错误。
func ContractNumberRuleKey(rule ContractNumberRule) (string, error) {
	normalized, err := NormalizeContractNumberRule(rule)
	if err != nil {
		return "", err
	}
	// 用 \x00 分隔字段，避免前缀/后缀拼接后产生同形字符串（如 "a"+"b" 与 "ab"+""）。
	material := normalized.Prefix + "\x00" + normalized.YearSegment + "\x00" +
		strconv.Itoa(normalized.SeqWidth) + "\x00" + normalized.Suffix
	digest := sha256.Sum256([]byte(material))
	return hex.EncodeToString(digest[:]), nil
}

// BuildContractNumber 按规则、年份和流水值拼装合同单号，不读写任何持久化状态。
// 参数：rule 为规则（内部先归一化校验），year 为生成时刻年份，sequence 为当年流水值（从 1 开始）。
// 返回值：最终单号，或 ErrNumberExhausted/ErrInvalidNumberRule 包装的错误。
func BuildContractNumber(rule ContractNumberRule, year int, sequence uint64) (string, error) {
	normalized, err := NormalizeContractNumberRule(rule)
	if err != nil {
		return "", err
	}
	if sequence == 0 {
		return "", fmt.Errorf("%w: 流水号必须从 1 开始", ErrInvalidNumberRule)
	}
	limit := ContractNumberSeqLimit(normalized.SeqWidth)
	if sequence > limit {
		return "", fmt.Errorf("%w: 流水号 %d 超出 %d 位流水的上限 %d，请调整流水位宽或更换前缀",
			ErrNumberExhausted, sequence, normalized.SeqWidth, limit)
	}
	number := normalized.Prefix + contractNumberYearPart(normalized.YearSegment, year) +
		fmt.Sprintf("%0*d", normalized.SeqWidth, sequence) + normalized.Suffix
	if len([]rune(number)) > ContractNumberMaxLength {
		return "", fmt.Errorf("%w: 合同单号长度 %d 超过 %d 个字符上限", ErrInvalidNumberRule,
			len([]rune(number)), ContractNumberMaxLength)
	}
	return number, nil
}

// ContractNumberSeqLimit 返回给定位宽可表示的最大流水值（如 4 位为 9999）。
func ContractNumberSeqLimit(width int) uint64 {
	if width < ContractNumberSeqWidthMin || width > ContractNumberSeqWidthMax {
		return 0
	}
	limit := uint64(1)
	for index := 0; index < width; index++ {
		limit *= 10
	}
	return limit - 1
}

// contractNumberYearPart 生成年份段；NONE 返回空串，年份取生成时刻的年份。
func contractNumberYearPart(segment string, year int) string {
	switch segment {
	case NumberYearSegmentYY:
		return fmt.Sprintf("%02d", ((year%100)+100)%100)
	case NumberYearSegmentYYYY:
		return fmt.Sprintf("%04d", year)
	default:
		return ""
	}
}

// contractNumberYearSegmentLength 返回年份段的字符数，用于最长单号估算。
func contractNumberYearSegmentLength(segment string) int {
	switch segment {
	case NumberYearSegmentYY:
		return 2
	case NumberYearSegmentYYYY:
		return 4
	default:
		return 0
	}
}
