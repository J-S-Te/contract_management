package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/j-s-te/contract-management/internal/application"
)

// TestImportStorageFailureResponse 保留容量依赖与在途上传的诊断语义且不泄漏底层错误。
// 参数：t为不访问业务库的HTTP信封测试上下文。
// 返回值：无；状态码、稳定错误码或错误脱敏不符时失败。
func TestImportStorageFailureResponse(t *testing.T) {
	for _, input := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: private-storage-path", application.ErrImportSourceStorageUnavailable), 503, "IMPORT_SOURCE_STORAGE_UNAVAILABLE"},
		{application.ErrImportSourceUploadPending, 409, "IMPORT_SOURCE_UPLOAD_PENDING"},
	} {
		t.Run(input.code, func(t *testing.T) {
			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)
			context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/contract-import-batches/batch/files", nil)
			writeError(context, input.err)
			body := response.Body.String()
			if response.Code != input.status || !strings.Contains(body, input.code) || strings.Contains(body, "private-storage-path") {
				t.Fatalf("存储错误信封不符: status=%d body=%s", response.Code, body)
			}
		})
	}
}

// TestImportSourceRejectedErrorIsDiagnosable 验证原件准入拒绝带出原因码而非通用校验文案。
// 参数：t为轻量gin+httptest测试上下文，不依赖数据库。
// 返回值：无；状态、错误码、原因码或文件名缺失时失败。
func TestImportSourceRejectedErrorIsDiagnosable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/contract-import-batches/batch-1/files", nil)
	writeError(context, &application.ImportSourceRejectedError{ReasonCode: "PDF_ACTIVE_CONTENT",
		Reason: "PDF含不支持的活动、嵌入或加密内容", Filename: "原件.pdf"})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "IMPORT_SOURCE_REJECTED") || !strings.Contains(body, "PDF_ACTIVE_CONTENT") ||
		!strings.Contains(body, "原件.pdf") || !strings.Contains(body, "PDF含不支持的活动、嵌入或加密内容") {
		t.Fatalf("body = %s, want reason code, filename and concrete message", body)
	}
	if strings.Contains(body, "请求参数不合法") {
		t.Fatalf("body = %s, must not fall back to the generic validation message", body)
	}
}

// TestImportSourceRetryableRejectionReturns503 验证可重试的准入拒绝返回503与Retry-After。
// 参数：t为轻量gin+httptest测试上下文。
// 返回值：无；状态、错误码或重试响应头缺失时失败。
func TestImportSourceRetryableRejectionReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = httptest.NewRequest(http.MethodPost, "/api/v1/contract-import-batches/batch-1/files", nil)
	writeError(context, &application.ImportSourceRejectedError{ReasonCode: "PDF_VALIDATION_BUSY",
		Reason: "PDF校验繁忙，请稍后重试", Filename: "原件.pdf", Retryable: true})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	if retryAfter := response.Header().Get("Retry-After"); retryAfter != "5" {
		t.Fatalf("Retry-After = %q, want 5", retryAfter)
	}
	if body := response.Body.String(); !strings.Contains(body, "IMPORT_SOURCE_BUSY") || !strings.Contains(body, "PDF_VALIDATION_BUSY") {
		t.Fatalf("body = %s, want busy code and reason code", body)
	}
}

// TestUploadImportSourceFormRejectionsAreDiagnosable 验证表单级422给出具体原因码与字段。
// 参数：t为不触达服务层的 handler 测试上下文。
// 返回值：无；错误码契约变化、原因码缺失或字段不明时失败。
func TestUploadImportSourceFormRejectionsAreDiagnosable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}
	missingVersionBody, missingVersionType := importUploadForm(t, "one", "")
	invalidVersionBody, invalidVersionType := importUploadForm(t, "one", "not-a-number")
	for _, input := range []struct {
		label, reasonCode, field string
		body                     []byte
		contentType              string
	}{
		{label: "multipart-invalid", reasonCode: "IMPORT_MULTIPART_INVALID", field: "file",
			body: []byte("not a multipart body"), contentType: "multipart/form-data; boundary=unclosed"},
		{label: "form-missing-version", reasonCode: "IMPORT_FORM_INVALID", field: "file",
			body: missingVersionBody, contentType: missingVersionType},
		{label: "version-invalid", reasonCode: "IMPORT_VERSION_INVALID", field: "expected_version",
			body: invalidVersionBody, contentType: invalidVersionType},
	} {
		t.Run(input.label, func(t *testing.T) {
			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)
			context.Request = httptest.NewRequest(http.MethodPost,
				"/api/v1/contract-import-batches/batch-1/files", bytes.NewReader(input.body))
			context.Request.Header.Set("Content-Type", input.contentType)
			context.Request.Form = nil
			handler.uploadImportSource(context)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, http.StatusUnprocessableEntity, response.Body.String())
			}
			details := importErrorDetails(t, response.Body.Bytes())
			if details["code"] != "CON_VALIDATION_ERROR" {
				t.Fatalf("code = %v, want CON_VALIDATION_ERROR", details["code"])
			}
			reason := details["reason_code"]
			if reason != input.reasonCode {
				t.Fatalf("reason_code = %v, want %s", reason, input.reasonCode)
			}
			field := details["field"]
			if field != input.field {
				t.Fatalf("field = %v, want %s", field, input.field)
			}
		})
	}
}

// importUploadForm 生成只含单个原件与可选版本字段的multipart请求体。
// 参数：t为测试上下文，itemKey为文本字段，version为空时省略expected_version。
// 返回值：multipart字节及其Content-Type。
func importUploadForm(t *testing.T, itemKey, version string) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("item_key", itemKey); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		if err := writer.WriteField("expected_version", version); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file", "原件.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("%PDF-1.7\n%%EOF")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

// importErrorDetails 解析错误信封中的code与details。
// 参数：t为测试上下文，body为响应体。
// 返回值：合并后的键值，便于逐项断言。
func importErrorDetails(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var envelope struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("响应不是合法信封: %v (%s)", err, body)
	}
	details := envelope.Details
	if details == nil {
		details = map[string]any{}
	}
	details["code"] = envelope.Code
	return details
}
