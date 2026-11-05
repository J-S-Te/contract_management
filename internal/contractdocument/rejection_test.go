package contractdocument

import (
	"errors"
	"testing"
)

// TestExternalSourceRejectionCodes 验证准入拒绝携带稳定原因码且文案逐字不变。
// 参数：t为文档准入测试上下文。
// 返回值：无；原因码错误、无法errors.As取出或文案被改写时失败。
func TestExternalSourceRejectionCodes(t *testing.T) {
	for _, input := range []struct {
		label, name, mime, code, message string
		body                             []byte
	}{
		{"unsupported-xlsx", "table.xlsx", "", CodeSourceTypeUnsupported, "源文件只支持DOCX或PDF", nil},
		{"unsupported-noext", "backup", "application/pdf", CodeSourceTypeUnsupported, "源文件只支持DOCX或PDF", nil},
		{"pdf-without-header", "broken.pdf", "application/pdf", CodePDFInvalid, "PDF为空、损坏或超过20MiB", []byte("not a pdf body")},
		{"pdf-empty", "empty.pdf", "application/pdf", CodePDFInvalid, "PDF为空、损坏或超过20MiB", nil},
		{"name-invalid", "bad\nname.pdf", "application/pdf", CodeSourceNameInvalid, "源文件名称无效", []byte("%PDF-1.4\n%%EOF")},
	} {
		t.Run(input.label, func(t *testing.T) {
			_, err := ValidateExternalSource(input.name, input.mime, input.body)
			rejection := externalSourceRejection(t, err)
			if rejection.Code != input.code {
				t.Fatalf("reason code = %q, want %q", rejection.Code, input.code)
			}
			if rejection.Message != input.message {
				t.Fatalf("message = %q, want %q", rejection.Message, input.message)
			}
			if rejection.Retryable {
				t.Fatal("确定性拒绝不应标记为可重试")
			}
		})
	}
}

// TestExternalPDFActiveContentRejectionCode 验证真实PDF中的活动对象被归类为活动内容。
// 参数：t为离线准入测试上下文。
// 返回值：无；活动对象被接受或原因归到其他规则时失败。
func TestExternalPDFActiveContentRejectionCode(t *testing.T) {
	body := admissionPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R /OpenAction << /S /JavaScript /JS (app.alert(1)) >> >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}, "")
	_, err := ValidateExternalSource("active.pdf", "application/pdf", body)
	rejection := externalSourceRejection(t, err)
	if rejection.Code != CodePDFActiveContent {
		t.Fatalf("reason code = %q, want %q", rejection.Code, CodePDFActiveContent)
	}
	if rejection.Message != "PDF含不支持的活动、嵌入或加密内容" {
		t.Fatalf("message = %q", rejection.Message)
	}
}

// TestRejectionRetryable 验证繁忙分支保留可重试标记与原始文案。
// 参数：t为测试上下文。
// 返回值：无；标记或文案丢失时失败。
func TestRejectionRetryable(t *testing.T) {
	rejection := externalSourceRejection(t, RejectRetryable(CodePDFValidationBusy, "PDF校验繁忙，请稍后重试"))
	if !rejection.Retryable || rejection.Code != CodePDFValidationBusy || rejection.Message != "PDF校验繁忙，请稍后重试" {
		t.Fatalf("unexpected retryable rejection: %+v", rejection)
	}
	if err := Reject(CodePDFInvalid, "PDF为空、损坏或超过20MiB"); err.Error() != "PDF为空、损坏或超过20MiB" {
		t.Fatalf("error text = %q", err.Error())
	}
}

// externalSourceRejection 断言错误可取出*RejectionError。
// 参数：t为测试上下文，err为待检查错误。
// 返回值：取出的拒绝错误；不可取出时终止测试。
func externalSourceRejection(t *testing.T, err error) *RejectionError {
	t.Helper()
	if err == nil {
		t.Fatal("非法原件被接受")
	}
	var rejection *RejectionError
	if !errors.As(err, &rejection) {
		t.Fatalf("错误未携带原因码: %v", err)
	}
	return rejection
}
