package application

import (
	"errors"
	"strings"

	"github.com/j-s-te/contract-management/internal/apperrors"
	"github.com/j-s-te/contract-management/internal/domain/contract"
)

// FeedbackIssue 是既有业务校验的安全字段定位，不保存或回显被拒绝值。
type FeedbackIssue struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message,omitempty"`
}

// BusinessFeedbackError 只承载代码显式指定的公开消息；内部错误保留在 Cause。
type BusinessFeedbackError struct {
	Cause      error
	Message    string
	ReasonCode string
	Issues     []FeedbackIssue
}

// Error 用途：返回显式安全的业务说明，不拼接内部错误。
// 参数：无。
// 返回值：用于当前业务拒绝的中文说明。
func (e *BusinessFeedbackError) Error() string { return e.Message }

// Unwrap 用途：保持原校验或状态错误的 errors.Is 分类。
// 参数：无。
// 返回值：原业务错误；仅供服务端分类和诊断。
func (e *BusinessFeedbackError) Unwrap() error { return e.Cause }

// PublicMessage 用途：明确标识可以传输到客户端的业务说明。
// 参数：无。
// 返回值：由业务判定分支构造的安全中文消息。
func (e *BusinessFeedbackError) PublicMessage() string { return e.Message }

// validationFeedback 用途：为现有参数拒绝增加安全字段和修正指引。
// 参数：field 为真实请求字段，rule 为既有规则，message 为代码指定的安全中文。
// 返回值：保持 ErrValidation 分类的公开业务错误。
func validationFeedback(field, rule, message string) error {
	return &BusinessFeedbackError{Cause: ErrValidation, Message: message,
		Issues: []FeedbackIssue{{Field: field, Rule: rule, Message: message}}}
}

// stateFeedback 用途：为既有状态拒绝保留明确的原因和后续动作。
// 参数：reason 为固定原因码，message 为当前拒绝分支的安全中文说明。
// 返回值：保持状态冲突分类的公开业务错误。
func stateFeedback(reason, message string) error {
	return &BusinessFeedbackError{Cause: apperrors.ErrStateConflict, Message: message, ReasonCode: reason}
}

// paymentTermsFeedback 用途：传递付款计划域显式公开的原因与行字段。
// 参数：err 为既有精确十进制付款计划校验错误。
// 返回值：保留应用和域分类的安全错误；未知错误不公开内部文本。
func paymentTermsFeedback(err error) error {
	var feedback *contract.PaymentTermsValidationError
	if errors.As(err, &feedback) {
		return &BusinessFeedbackError{Cause: errors.Join(ErrValidation, err), Message: feedback.PublicMessage(),
			Issues: []FeedbackIssue{{Field: feedback.FieldPath(), Rule: feedback.Rule, Message: feedback.PublicMessage()}}}
	}
	return validationFeedback("payment_terms", "invalid", "付款计划未通过校验，请核对分期号、日期、金额及合计后重新保存。")
}

// contractShapeFeedback 用途：说明已被现有合同形状规则拒绝的具体输入。
// 参数：c 为已拒绝合同，external 区分外部合同与模板合同。
// 返回值：首个可定位的安全业务原因，不新增或执行写入门禁。
func contractShapeFeedback(c contract.Contract, external bool) error {
	switch {
	case c.Title == "":
		return validationFeedback("title", "required", "请填写合同名称后保存。")
	case c.Type == "":
		return validationFeedback("contract_type", "required", "请选择合同类型后保存。")
	case c.AmountMinor < 0 || external && c.AmountMinor <= 0:
		return validationFeedback("amount_minor", "min", "合同金额不符合要求，外部合同金额须大于零，模板合同金额不能为负数。")
	case c.Content == "" && len(c.Document) == 0:
		return validationFeedback("content", "required", "合同正文或原件缺失，请重新选择模板或上传原件。")
	case external && c.TemplateID != "":
		return validationFeedback("template_id", "invalid", "外部合同不能同时关联模板，请重新选择合同来源后提交。")
	case external && c.Type != "直签" && c.Type != "三方":
		return validationFeedback("contract_type", "enum", "外部合同类型仅支持直签或三方，请重新选择。")
	case !external && c.TemplateID == "":
		return validationFeedback("template_id", "required", "请选择可用合同模板后保存。")
	case !external && c.OpportunityID != "" && c.CRMCustomerID == 0:
		return validationFeedback("crm_customer_id", "required", "关联商机时必须选择其所属客户，请重新选择客户与商机。")
	default:
		return validationFeedback("service_items", "invalid", "服务项不符合要求，请检查服务类型、实施场所、批次、检测类别、检测方式及系统名称和等级。")
	}
}

// signingShipmentFeedback 用途：定位已经命中快递信息长度拒绝的真实字段。
// 参数：shipment 为现有快递表单，不保存或回显输入内容。
// 返回值：明确字段上限及修正动作的安全校验错误。
func signingShipmentFeedback(shipment contract.SigningShipment) error {
	if len([]rune(strings.TrimSpace(shipment.CourierNumber))) > 120 {
		return validationFeedback("courier_number", "max", "快递单号不能超过 120 个字符，请核对后重新填写。")
	}
	if len([]rune(strings.TrimSpace(shipment.RecipientName))) > 120 {
		return validationFeedback("recipient_name", "max", "收件人姓名不能超过 120 个字符，请缩短后保存。")
	}
	return validationFeedback("recipient_address", "max", "收件地址不能超过 500 个字符，请缩短后保存。")
}
