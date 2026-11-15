package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/application"
)

// TestContractDecodeFeedbackClassifiesProtocolErrors 用途：防止 JSON 协议错误误报为用户必填错误。
// 参数：t 为测试上下文；测试真实 HTTP 解码器且不调用业务服务。
// 返回值：无；回执应保持错误码并只包含受控原因。
func TestContractDecodeFeedbackClassifiesProtocolErrors(t *testing.T) {
	for _, item := range []struct{ body, reason string }{
		{`{"extra":"do-not-echo"}`, "unknown_field"},
		{`{"version":"do-not-echo"}`, "invalid_type"},
		{`{"version":`, "invalid_json"},
		{`{} {}`, "invalid_json"},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/contracts", strings.NewReader(item.body))
		var input struct {
			Version uint64 `json:"version"`
		}
		if decode(ctx, &input) {
			t.Fatal("invalid request was accepted")
		}
		var body struct {
			Code    string
			Details struct{ Reason string }
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("unsafe diagnostic shape: %v; %s", err, recorder.Body)
		}
		if recorder.Code != 422 || body.Code != "CON_VALIDATION_ERROR" || body.Details.Reason != item.reason {
			t.Fatalf("response=%d %s", recorder.Code, recorder.Body)
		}
		if strings.Contains(recorder.Body.String(), "do-not-echo") || strings.Contains(recorder.Body.String(), "json:") {
			t.Fatalf("parser detail leaked: %s", recorder.Body)
		}
	}
}

// TestContractFeedbackPublishesOnlyExplicitMessages 用途：显式业务原因可见，内部及普通包装错误不可见。
// 参数：t 为测试上下文；原错误分类、状态和业务码必须保留。
// 返回值：无；字段、原因码或内部异常泄漏时失败。
func TestContractFeedbackPublishesOnlyExplicitMessages(t *testing.T) {
	for _, item := range []struct {
		err            error
		status         int
		code, fragment string
	}{
		{&application.BusinessFeedbackError{Cause: errors.Join(application.ErrValidation, errors.New("password=do-not-echo")), Message: "金额必须大于零，请重新填写。", Issues: []application.FeedbackIssue{{Field: "amount_minor", Rule: "min", Message: "金额必须大于零，请重新填写。"}}}, 422, "CON_VALIDATION_ERROR", "amount_minor"},
		{&application.BusinessFeedbackError{Cause: apperrors.ErrStateConflict, Message: "审批节点尚未全部通过，请先核对处理结果。", ReasonCode: "APPROVAL_NODES_NOT_APPROVED"}, 409, "CON_STATE_CONFLICT", "APPROVAL_NODES_NOT_APPROVED"},
		{fmt.Errorf("%w: password=do-not-echo", application.ErrValidation), 422, "CON_VALIDATION_ERROR", "请求参数不合法"},
		{errors.New("SQLSTATE password=do-not-echo"), 500, "CON_INTERNAL_ERROR", "服务暂时不可用"},
		{errors.Join(application.ErrForbidden, &application.BusinessFeedbackError{Cause: application.ErrValidation, Message: "do-not-echo"}), 403, "AUTH_FORBIDDEN", "无权"},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/contracts", nil)
		writeError(ctx, item.err)
		if recorder.Code != item.status || !strings.Contains(recorder.Body.String(), item.code) || !strings.Contains(recorder.Body.String(), item.fragment) || strings.Contains(recorder.Body.String(), "do-not-echo") || strings.Contains(recorder.Body.String(), "SQLSTATE") {
			t.Fatalf("unsafe or incomplete response: %d %s", recorder.Code, recorder.Body)
		}
	}
}

// TestContractCommercialFeedbackReachesHTTP 用途：防止真实应用校验的公开原因在 HTTP 映射中被丢弃。
// 参数：t 为测试上下文；非法币种通过现有外部合同创建链路拒绝。
// 返回值：无；断言未持久化且回执说明币种问题。
func TestContractCommercialFeedbackReachesHTTP(t *testing.T) {
	t.Setenv("APP_PUBLIC_URL", "http://example.com/contract_management")
	repo := &externalContractRepository{}
	metadata := consistencyMetadata()
	metadata["currency"] = "123"
	metadata["payment_terms"] = []any{}
	request := externalContractUploadRequest(t, consistencyJSON(t, metadata), "external.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", externalContractDOCX(t))
	recorder := httptest.NewRecorder()
	service := &application.Service{Repo: repo, DetectionCategories: externalDetectionCategoryDirectory{}, CRMContractReferences: externalCRMReferenceDirectory{}}
	consistencyRouter(service).ServeHTTP(recorder, request)
	if recorder.Code != 422 || repo.created.ID != "" || !strings.Contains(recorder.Body.String(), "币种") {
		t.Fatalf("business reason lost: status=%d body=%s", recorder.Code, recorder.Body)
	}
}
