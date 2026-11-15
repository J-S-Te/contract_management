package httpapi

import (
	"encoding/json"
	"errors"
	"strings"
)

// decodeFailureReason 用途：将严格 JSON 解码故障分类，不公开解析器文本或输入值。
// 参数：err 为现有 JSON 解码器返回的错误。
// 返回值：固定协议原因 unknown_field、invalid_type 或 invalid_json。
func decodeFailureReason(err error) string {
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) {
		return "invalid_type"
	}
	if err != nil && strings.HasPrefix(err.Error(), "json: unknown field ") {
		return "unknown_field"
	}
	return "invalid_json"
}
