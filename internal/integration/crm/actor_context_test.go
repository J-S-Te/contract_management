package crm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/j-s-te/contract-management/internal/application"
)

// TestActorContextFailureIsNotInvalidCustomer 验证明确上下文错误不会误报客户字段无效。
// 参数：t为测试执行器。
// 返回值：无；错误被合并、信任自由文本或错误状态时失败。
func TestActorContextFailureIsNotInvalidCustomer(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		context    bool
	}{
		{"explicit-context-code", `{"code":"CRM_CONTRACT_REFERENCE_ACTOR_CONTEXT_REQUIRED"}`, 422, true},
		{"invalid-customer", `{"code":"CRM_CONTRACT_REFERENCE_INVALID"}`, 422, false},
		{"untrusted-message", `{"message":"CRM_CONTRACT_REFERENCE_ACTOR_CONTEXT_REQUIRED"}`, 422, false},
		{"wrong-status", `{"code":"CRM_CONTRACT_REFERENCE_ACTOR_CONTEXT_REQUIRED"}`, 403, false},
		{"trailing-json", `{"code":"CRM_CONTRACT_REFERENCE_ACTOR_CONTEXT_REQUIRED"} {}`, 422, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			directory := &ContractReferenceDirectory{BaseURL: "https://crm.example",
				TokenSource: func(context.Context) (string, error) { return "test-token", nil },
				Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: tt.status,
						Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
				})}}
			_, err := directory.Resolve(context.Background(), 7, "", "actor-1")
			actual := errors.Is(err, application.ErrCRMActorContextRequired)
			if actual != tt.context {
				t.Fatalf("context=%v want=%v err=%v", actual, tt.context, err)
			}
			if tt.context && errors.Is(err, application.ErrCRMReferenceInvalid) {
				t.Fatal("缺授权上下文不能表示客户无效")
			}
			if tt.context && !errors.Is(err, application.ErrValidation) {
				t.Fatal("必须保留普通接口的422校验错误兼容性")
			}
		})
	}
}
