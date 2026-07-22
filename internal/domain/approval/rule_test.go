package approval

import "testing"

func TestMatchHighestRule(t *testing.T) {
	t.Parallel()
	rules := []Rule{
		{ID: "low", Priority: 10, Enabled: true, Expression: Expression{Conditions: []Condition{{Field: FieldAmountMinor, Operator: OperatorLTE, Value: int64(5_000_000)}}}},
		{ID: "high", Priority: 100, Enabled: true, Expression: Expression{Conditions: []Condition{{Field: FieldCustomerCredit, Operator: OperatorEQ, Value: "A"}}}},
	}
	rule, err := MatchHighest(rules, Facts{AmountMinor: 2_000_000, CustomerCreditLevel: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if rule == nil || rule.ID != "high" {
		t.Fatalf("expected high priority rule, got %#v", rule)
	}
}

func TestNestedRuleExpression(t *testing.T) {
	t.Parallel()
	expr := Expression{Logical: LogicalAnd, Conditions: []Condition{{Field: FieldAmountMinor, Operator: OperatorLTE, Value: float64(10_000_000)}}, Groups: []Expression{{Logical: LogicalOr, Conditions: []Condition{{Field: FieldServiceType, Operator: OperatorEQ, Value: "标准服务"}, {Field: FieldContractType, Operator: OperatorEQ, Value: "续签"}}}}}
	matched, err := expr.Match(Facts{AmountMinor: 8_000_000, ServiceType: "标准服务"})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected nested expression to match")
	}
}
