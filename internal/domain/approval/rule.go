package approval

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

type LogicalOperator string
type Field string
type Operator string

const (
	LogicalAnd LogicalOperator = "and"
	LogicalOr  LogicalOperator = "or"

	FieldAmountMinor    Field = "amount_minor"
	FieldServiceType    Field = "service_type"
	FieldCustomerCredit Field = "customer_credit_level"
	FieldContractType   Field = "contract_type"
	FieldTermsIdentical Field = "terms_identical"

	OperatorEQ  Operator = "eq"
	OperatorNE  Operator = "ne"
	OperatorGT  Operator = "gt"
	OperatorGTE Operator = "gte"
	OperatorLT  Operator = "lt"
	OperatorLTE Operator = "lte"
	OperatorIn  Operator = "in"
)

// 审批规则的规模上限。表达式按节点树递归匹配，规则按天被重复求值，
// 因此必须在保存时限制节点、条件组、条件与嵌套深度，
// 避免单条规则把匹配成本放大到不可控。
const (
	MaxRuleNodes            = 20
	MaxExpressionGroups     = 50
	MaxExpressionConditions = 50
	MaxExpressionDepth      = 8
)

var ErrInvalidExpression = errors.New("invalid approval rule expression")

type Expression struct {
	Logical    LogicalOperator `json:"logical,omitempty"`
	Conditions []Condition     `json:"conditions,omitempty"`
	Groups     []Expression    `json:"groups,omitempty"`
}

type Condition struct {
	Field    Field    `json:"field"`
	Operator Operator `json:"operator"`
	Value    any      `json:"value"`
}

type Facts struct {
	AmountMinor         int64
	ServiceType         string
	CustomerCreditLevel string
	ContractType        string
	TermsIdentical      bool
}

// MatchHighest 为工作流输入选择一个不可变规则快照。
func MatchHighest(rules []Rule, facts Facts) (*Rule, error) {
	candidates := append([]Rule(nil), rules...)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Priority > candidates[j].Priority })
	for i := range candidates {
		if !candidates[i].Enabled {
			continue
		}
		matched, err := candidates[i].Expression.Match(facts)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", candidates[i].ID, err)
		}
		if matched {
			return &candidates[i], nil
		}
	}
	return nil, nil
}

func (e Expression) Match(f Facts) (bool, error) {
	logical := e.Logical
	if logical == "" {
		logical = LogicalAnd
	}
	if logical != LogicalAnd && logical != LogicalOr {
		return false, fmt.Errorf("%w: logical operator %q", ErrInvalidExpression, logical)
	}
	results := make([]bool, 0, len(e.Conditions)+len(e.Groups))
	for _, condition := range e.Conditions {
		matched, err := condition.match(f)
		if err != nil {
			return false, err
		}
		results = append(results, matched)
	}
	for _, group := range e.Groups {
		matched, err := group.Match(f)
		if err != nil {
			return false, err
		}
		results = append(results, matched)
	}
	if len(results) == 0 {
		return false, fmt.Errorf("%w: empty group", ErrInvalidExpression)
	}
	if logical == LogicalAnd {
		for _, matched := range results {
			if !matched {
				return false, nil
			}
		}
		return true, nil
	}
	for _, matched := range results {
		if matched {
			return true, nil
		}
	}
	return false, nil
}

// ValidateLimits 校验表达式的规模与嵌套深度上限。
// 参数：无；从根表达式开始递归统计，根组本身计为一个条件组。
// 返回值：超出条件组数、条件数或嵌套深度上限时返回 ErrInvalidExpression。
func (e Expression) ValidateLimits() error {
	counts := expressionLimits{}
	return e.walkLimits(1, &counts)
}

// expressionLimits 累计递归遍历中的条件组与条件数量。
type expressionLimits struct {
	groups     int
	conditions int
}

// walkLimits 递归统计并即时失败，避免超限规则在保存前完整展开。
// 参数：depth为当前组深度（根为1），counts为累计计数。
// 返回值：超出上限时返回 ErrInvalidExpression。
func (e Expression) walkLimits(depth int, counts *expressionLimits) error {
	if depth > MaxExpressionDepth {
		return fmt.Errorf("%w: 条件组嵌套深度不能超过 %d 层", ErrInvalidExpression, MaxExpressionDepth)
	}
	counts.groups++
	counts.conditions += len(e.Conditions)
	if counts.groups > MaxExpressionGroups || counts.conditions > MaxExpressionConditions {
		return fmt.Errorf("%w: 条件组不能超过 %d 个、条件不能超过 %d 个", ErrInvalidExpression,
			MaxExpressionGroups, MaxExpressionConditions)
	}
	for _, group := range e.Groups {
		if err := group.walkLimits(depth+1, counts); err != nil {
			return err
		}
	}
	return nil
}

func (c Condition) match(f Facts) (bool, error) {
	switch c.Field {
	case FieldAmountMinor:
		value, ok := numberToInt64(c.Value)
		if !ok {
			return false, fmt.Errorf("%w: amount value", ErrInvalidExpression)
		}
		return compareInt(f.AmountMinor, value, c.Operator)
	case FieldTermsIdentical:
		value, ok := c.Value.(bool)
		if !ok || (c.Operator != OperatorEQ && c.Operator != OperatorNE) {
			return false, fmt.Errorf("%w: boolean condition", ErrInvalidExpression)
		}
		return (f.TermsIdentical == value) == (c.Operator == OperatorEQ), nil
	case FieldServiceType, FieldCustomerCredit, FieldContractType:
		actual := map[Field]string{FieldServiceType: f.ServiceType, FieldCustomerCredit: f.CustomerCreditLevel, FieldContractType: f.ContractType}[c.Field]
		return compareString(actual, c.Value, c.Operator)
	default:
		return false, fmt.Errorf("%w: field %q", ErrInvalidExpression, c.Field)
	}
}

func numberToInt64(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), float64(int64(n)) == n
	default:
		return 0, false
	}
}

func compareInt(actual, expected int64, op Operator) (bool, error) {
	switch op {
	case OperatorEQ:
		return actual == expected, nil
	case OperatorNE:
		return actual != expected, nil
	case OperatorGT:
		return actual > expected, nil
	case OperatorGTE:
		return actual >= expected, nil
	case OperatorLT:
		return actual < expected, nil
	case OperatorLTE:
		return actual <= expected, nil
	default:
		return false, fmt.Errorf("%w: numeric operator %q", ErrInvalidExpression, op)
	}
}

func compareString(actual string, expected any, op Operator) (bool, error) {
	actual = strings.TrimSpace(actual)
	switch op {
	case OperatorEQ, OperatorNE:
		value, ok := expected.(string)
		if !ok {
			return false, fmt.Errorf("%w: string value", ErrInvalidExpression)
		}
		matched := actual == strings.TrimSpace(value)
		return matched == (op == OperatorEQ), nil
	case OperatorIn:
		values, ok := expected.([]string)
		if !ok {
			if raw, cast := expected.([]any); cast {
				values = make([]string, 0, len(raw))
				for _, item := range raw {
					value, cast := item.(string)
					if !cast {
						return false, fmt.Errorf("%w: in value", ErrInvalidExpression)
					}
					values = append(values, value)
				}
			} else {
				return false, fmt.Errorf("%w: in value", ErrInvalidExpression)
			}
		}
		for _, value := range values {
			if actual == strings.TrimSpace(value) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: string operator %q", ErrInvalidExpression, op)
	}
}
