package application

import (
	"errors"
	"strings"
	"testing"

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
