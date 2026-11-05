package contractdocument

// 原件准入原因码：稳定、可枚举，供上层映射为可诊断的HTTP错误与日志字段。
// 只描述被拒的规则，不携带原件字节、路径或客户敏感信息。
const (
	CodeSourceTypeUnsupported = "SOURCE_TYPE_UNSUPPORTED"
	CodeSourceNameInvalid     = "SOURCE_NAME_INVALID"
	CodeSourceMIMEInvalid     = "SOURCE_MIME_INVALID"
	CodeSourceMIMEMismatch    = "SOURCE_MIME_MISMATCH"

	CodePDFInvalid          = "PDF_INVALID"
	CodePDFStructureInvalid = "PDF_STRUCTURE_INVALID"
	CodePDFEncrypted        = "PDF_ENCRYPTED"
	CodePDFActiveContent    = "PDF_ACTIVE_CONTENT"
	CodePDFImageBudget      = "PDF_IMAGE_BUDGET_EXCEEDED"
	CodePDFObjectBudget     = "PDF_OBJECT_BUDGET_EXCEEDED"
	CodePDFPageStructure    = "PDF_PAGE_STRUCTURE_INVALID"
	CodePDFValidationBusy   = "PDF_VALIDATION_BUSY"

	CodeDOCXInvalid   = "DOCX_INVALID"
	CodeDOCXEmptyText = "DOCX_EMPTY_TEXT"
)

// RejectionError 携带原因码的原件拒绝错误。
// 参数：Code为稳定的机器可读原因码，Message为面向用户的中文原始文案，
// Retryable表示同一原件稍后重试可能通过（例如校验并发繁忙）。
type RejectionError struct {
	Code      string
	Message   string
	Retryable bool
}

// Error 返回面向用户的原始拒绝文案，保持既有错误文本不变。
// 参数：无。
// 返回值：中文或原始依赖文案。
func (e *RejectionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// Reject 构造不可通过重试消除的拒绝错误。
// 参数：code为原因码，message为保持既有文案的拒绝原因。
// 返回值：可被errors.As取出的*RejectionError。
func Reject(code, message string) error {
	return &RejectionError{Code: code, Message: message}
}

// RejectRetryable 构造可稍后重试的拒绝错误。
// 参数：code为原因码，message为保持既有文案的拒绝原因。
// 返回值：Retryable为true的*RejectionError。
func RejectRetryable(code, message string) error {
	return &RejectionError{Code: code, Message: message, Retryable: true}
}
