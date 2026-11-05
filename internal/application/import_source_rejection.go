package application

import (
	"errors"
	"strings"

	"github.com/j-s-te/contract-management/internal/contractdocument"
)

// 导入原件拒绝原因码：服务层可诊断拒绝的稳定标识，不携带原件内容。
const (
	CodeImportItemKeyInvalid = "IMPORT_ITEM_KEY_INVALID"
	CodeImportSourceLimit    = "IMPORT_SOURCE_LIMIT"
	CodeSourceRejected       = "SOURCE_REJECTED"
)

// ImportSourceRejectedError 描述一次可诊断的原件准入拒绝。
// 参数：ReasonCode为稳定原因码，Reason为面向用户的原始拒绝文案，
// Filename为已规范化的原件名，Retryable表示稍后重试同一原件可能通过。
// 该类型Unwrap到ErrValidation，保持既有errors.Is判定与422语义。
type ImportSourceRejectedError struct {
	ReasonCode string
	Reason     string
	Filename   string
	Retryable  bool
}

// Error 返回原始拒绝文案。
// 参数：无。
// 返回值：面向用户的中文或依赖原始文案。
func (e *ImportSourceRejectedError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason
}

// Unwrap 保留ErrValidation身份，使既有errors.Is/IsValidation调用不受影响。
// 参数：无。
// 返回值：包级ErrValidation。
func (e *ImportSourceRejectedError) Unwrap() error { return ErrValidation }

// importSourceRejected 把原件准入错误转换为可诊断拒绝。
// 参数：err为准入错误，name为上传声明的原件名（可能不可信）。
// 返回值：带原因码的*ImportSourceRejectedError；非RejectionError时使用兜底码。
func importSourceRejected(err error, name string) error {
	filename := importSourceFilename(name)
	var rejection *contractdocument.RejectionError
	if errors.As(err, &rejection) {
		return &ImportSourceRejectedError{ReasonCode: rejection.Code, Reason: rejection.Message,
			Filename: filename, Retryable: rejection.Retryable}
	}
	code := CodeSourceRejected
	message := "原件未通过准入校验"
	if err != nil {
		message = err.Error()
	}
	return &ImportSourceRejectedError{ReasonCode: code, Reason: message, Filename: filename}
}

// importSourceFilename 规范化原件名，仅保留定位所需的可打印基名。
// 参数：name为上传声明或库内文件名，视为不可信输入。
// 返回值：去除目录部分、控制字符且不超过255字符的名称。
func importSourceFilename(name string) string {
	if index := strings.LastIndexAny(name, "/\\"); index >= 0 {
		name = name[index+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r == 0 || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if runes := []rune(name); len(runes) > 255 {
		name = string(runes[:255])
	}
	return name
}
