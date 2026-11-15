package application

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/approval"
	"github.com/j-s-te/contract-management/internal/domain/contract"
	"github.com/j-s-te/contract-management/internal/workflows"
	"go.temporal.io/api/enums/v1"
)

// TestContractValidationFeedbackIsExplicitlyPublic 用途：校验拒绝必须带显式公开载体且保持错误分类。
// 参数：t 为测试上下文；使用合同币种及字段长度边界。
// 返回值：无；缺失安全公开消息时失败。
func TestContractValidationFeedbackIsExplicitlyPublic(t *testing.T) {
	_, currencyErr := finalizeContractCommercials(contract.Contract{Currency: "123", ServiceItems: []contract.ServiceItem{{ServiceType: "x"}}})
	lengthErr := validateContractTextLengths(contract.Contract{Title: strings.Repeat("长", 256)})
	for _, item := range []struct {
		err             error
		field, fragment string
	}{
		{currencyErr, "currency", "币种"}, {lengthErr, "title", "合同名称"},
	} {
		if !errors.Is(item.err, ErrValidation) {
			t.Fatalf("classification lost: %v", item.err)
		}
		var feedback *BusinessFeedbackError
		if !errors.As(item.err, &feedback) || !strings.Contains(feedback.PublicMessage(), item.fragment) ||
			len(feedback.Issues) != 1 || feedback.Issues[0].Field != item.field {
			t.Fatalf("safe field feedback absent: %v", item.err)
		}
	}
}

// TestContractFinancialFeedbackPreservesPlanRowsAndAuthorization 用途：财务变更错误须定位付款行且不能替代权限拒绝。
// 参数：t 为测试上下文；使用现有内存仓储夹具，无外部业务写入。
// 返回值：无；真实行、原因或权限分类不符时失败。
func TestContractFinancialFeedbackPreservesPlanRowsAndAuthorization(t *testing.T) {
	service, repo, actor := financialChangeFixture()
	change := validFinancialChange()
	change.PaymentTerms[1].DueDate = "2027-02-29"
	_, err := service.ChangeContractFinancialTerms(context.Background(), actor, "C1", change)
	var feedback *BusinessFeedbackError
	if !errors.As(err, &feedback) || len(feedback.Issues) != 1 || feedback.Issues[0].Field != "payment_terms[1].due_date" || !strings.Contains(feedback.PublicMessage(), "第2期") {
		t.Fatalf("plan row lost: %v", err)
	}
	actor.Permissions = nil
	_, err = service.ChangeContractFinancialTerms(context.Background(), actor, "C1", change)
	if !errors.Is(err, ErrForbidden) || errors.As(err, &feedback) || len(repo.applied) != 0 {
		t.Fatalf("permission or no-write boundary changed: %v", err)
	}
}

// TestContractRuleAndReplacementFeedbackRemainsActionable 用途：防止其他现有中文校验在 HTTP 层退化。
// 参数：t 为测试上下文；只触发保存前的规则和文件更换参数拒绝。
// 返回值：无；公开错误必须保留字段定位与原校验分类。
func TestContractRuleAndReplacementFeedbackRemainsActionable(t *testing.T) {
	_, replacementErr := (&Service{}).ReplaceStampedDocument(context.Background(), Principal{}, "", "", nil, 1, "")
	ruleErr := validateRule(approval.Rule{Name: strings.Repeat("长", 129), Nodes: []approval.Node{{ID: "one"}}})
	for _, err := range []error{replacementErr, ruleErr} {
		feedback, ok := err.(interface{ PublicMessage() string })
		if !errors.Is(err, ErrValidation) || !ok || feedback.PublicMessage() == "" {
			t.Fatalf("public rejection absent: %v", err)
		}
	}
}

// TestContractRecoveryFeedbackExplainsUnfinishedWorkflow 用途：防止收口拒绝丢失等待或人工复核指引。
// 参数：t 为测试上下文；工作流运行中且无业务写入。
// 返回值：无；保留状态冲突并提供安全公开原因。
func TestContractRecoveryFeedbackExplainsUnfinishedWorkflow(t *testing.T) {
	service, repo := recoveryService(t, enums.WORKFLOW_EXECUTION_STATUS_RUNNING, workflows.ApprovalState{})
	err := service.RecoverApproval(context.Background(), recoveryActor(), "approval-1")
	feedback, ok := err.(interface{ PublicMessage() string })
	if !errors.Is(err, apperrors.ErrStateConflict) || !ok || !strings.Contains(feedback.PublicMessage(), "等待") {
		t.Fatalf("recovery diagnosis absent: %v", err)
	}
	if repo.completed != nil {
		t.Fatal("rejected recovery must not complete approval")
	}
}
