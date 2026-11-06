package application

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// 页面 maxlength 只是第一道限制；服务端必须用同一组列宽拒绝绕过页面的超长输入。
func TestValidateContractTextLengthsMatchesDatabaseColumns(t *testing.T) {
	long := strings.Repeat("测", 256)
	cases := []contract.Contract{
		{Title: long},
		{CustomerAddress: strings.Repeat("址", 501)},
		{CustomerContact: strings.Repeat("人", 129)},
		{CustomerPhone: strings.Repeat("1", 65)},
		{ServiceItems: []contract.ServiceItem{{Name: long}}},
		{ServiceItems: []contract.ServiceItem{{Requirement: strings.Repeat("求", 1001)}}},
		{TemplateValues: map[string]string{"备注": strings.Repeat("值", 2001)}},
	}
	for index, item := range cases {
		if err := validateContractTextLengths(item); !errors.Is(err, ErrValidation) {
			t.Fatalf("case %d error = %v, want ErrValidation", index, err)
		}
	}
	valid := contract.Contract{
		Title: strings.Repeat("合", 255), CustomerAddress: strings.Repeat("址", 500),
		ServiceItems:   []contract.ServiceItem{{Name: strings.Repeat("服", 255), Requirement: strings.Repeat("求", 1000)}},
		TemplateValues: map[string]string{"备注": strings.Repeat("值", 2000)},
	}
	if err := validateContractTextLengths(valid); err != nil {
		t.Fatalf("boundary values must be accepted: %v", err)
	}
}

func TestValidateRuleRejectsOverlongNameAndNode(t *testing.T) {
	base := approval.Rule{
		Name: "规则",
		Expression: approval.Expression{Logical: approval.LogicalAnd, Conditions: []approval.Condition{{
			Field: approval.FieldAmountMinor, Operator: approval.OperatorLTE, Value: int64(100),
		}}},
		Nodes: []approval.Node{{ID: "node-1", Name: "审批", RoleCode: "sales_director"}},
	}
	if err := validateRule(base); err != nil {
		t.Fatalf("valid rule rejected: %v", err)
	}
	overlongName := base
	overlongName.Name = strings.Repeat("规", 129)
	if err := validateRule(overlongName); !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong rule name error = %v", err)
	}
	overlongNode := base
	overlongNode.Nodes = []approval.Node{{ID: "node-1", Name: strings.Repeat("节", 129), RoleCode: "sales_director"}}
	if err := validateRule(overlongNode); !errors.Is(err, ErrValidation) {
		t.Fatalf("overlong node name error = %v", err)
	}
}

// TestValidateRuleEnforcesSizeAndTimeoutCaps 验证审批规则的节点数、条件组数、条件数、
// 嵌套深度与节点时限都按显式上限校验，且边界值仍被接受。
// 参数：t为测试上下文。
// 返回值：无；超限规则被接受或边界合法规则被拒时失败。
func TestValidateRuleEnforcesSizeAndTimeoutCaps(t *testing.T) {
	condition := approval.Condition{Field: approval.FieldAmountMinor, Operator: approval.OperatorLTE, Value: int64(100)}
	nodes := func(count int) []approval.Node {
		result := make([]approval.Node, 0, count)
		for index := 0; index < count; index++ {
			result = append(result, approval.Node{ID: fmt.Sprintf("node-%d", index+1), Name: "审批", RoleCode: "sales_director"})
		}
		return result
	}
	flat := func(conditions int) approval.Expression {
		list := make([]approval.Condition, conditions)
		for index := range list {
			list[index] = condition
		}
		return approval.Expression{Logical: approval.LogicalAnd, Conditions: list}
	}
	grouped := func(groups int) approval.Expression {
		expression := approval.Expression{Logical: approval.LogicalAnd}
		for index := 0; index < groups; index++ {
			expression.Groups = append(expression.Groups, approval.Expression{Logical: approval.LogicalAnd, Conditions: []approval.Condition{condition}})
		}
		return expression
	}
	nested := func(depth int) approval.Expression {
		expression := approval.Expression{Logical: approval.LogicalAnd, Conditions: []approval.Condition{condition}}
		for level := 1; level < depth; level++ {
			expression = approval.Expression{Logical: approval.LogicalAnd, Conditions: []approval.Condition{condition}, Groups: []approval.Expression{expression}}
		}
		return expression
	}
	timed := func(timeout time.Duration) []approval.Node {
		list := nodes(1)
		list[0].Timeout = timeout
		return list
	}

	cases := []struct {
		name      string
		rule      approval.Rule
		wantError bool
	}{
		{"节点数达到上限", approval.Rule{Name: "规则", Expression: flat(1), Nodes: nodes(approval.MaxRuleNodes)}, false},
		{"节点数超限", approval.Rule{Name: "规则", Expression: flat(1), Nodes: nodes(approval.MaxRuleNodes + 1)}, true},
		{"条件数达到上限", approval.Rule{Name: "规则", Expression: flat(approval.MaxExpressionConditions), Nodes: nodes(1)}, false},
		{"条件数超限", approval.Rule{Name: "规则", Expression: flat(approval.MaxExpressionConditions + 1), Nodes: nodes(1)}, true},
		{"条件组数达到上限", approval.Rule{Name: "规则", Expression: grouped(approval.MaxExpressionGroups - 1), Nodes: nodes(1)}, false},
		{"条件组数超限", approval.Rule{Name: "规则", Expression: grouped(approval.MaxExpressionGroups), Nodes: nodes(1)}, true},
		{"嵌套深度达到上限", approval.Rule{Name: "规则", Expression: nested(approval.MaxExpressionDepth), Nodes: nodes(1)}, false},
		{"嵌套深度超限", approval.Rule{Name: "规则", Expression: nested(approval.MaxExpressionDepth + 1), Nodes: nodes(1)}, true},
		{"未配置时限沿用默认", approval.Rule{Name: "规则", Expression: flat(1), Nodes: timed(0)}, false},
		{"时限于下界", approval.Rule{Name: "规则", Expression: flat(1), Nodes: timed(approval.MinNodeTimeout)}, false},
		{"时限于上界", approval.Rule{Name: "规则", Expression: flat(1), Nodes: timed(approval.MaxNodeTimeout)}, false},
		{"时限过短", approval.Rule{Name: "规则", Expression: flat(1), Nodes: timed(approval.MinNodeTimeout - time.Second)}, true},
		{"时限过长", approval.Rule{Name: "规则", Expression: flat(1), Nodes: timed(approval.MaxNodeTimeout + time.Second)}, true},
	}
	for _, test := range cases {
		err := validateRule(test.rule)
		if test.wantError && !errors.Is(err, ErrValidation) {
			t.Errorf("%s: error = %v, want ErrValidation", test.name, err)
		}
		if !test.wantError && err != nil {
			t.Errorf("%s: 边界合法规则被拒: %v", test.name, err)
		}
	}
}
