package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type ImportTargetTransport struct {
	BaseURL     string
	TokenSource func(context.Context) (string, error)
	Client      *http.Client
}

type ImportTargetHTTPError struct {
	Status int
	Code   string
}

// Error 返回确定拒绝分类，不复制接收方原始错误正文。
// 参数：无。
// 返回值：固定公开错误码。
func (e *ImportTargetHTTPError) Error() string { return "IMPORT_TARGET_REJECTED" }

// Is 将确定业务拒绝保留为失败关闭分类。
// 参数：target为错误比较目标。
// 返回值：仅匹配无效目标回执类别。
func (e *ImportTargetHTTPError) Is(target error) bool { return target == ErrImportTargetReceipt }

type ImportTargetWaitError struct{ Code string }

// Error 返回公开的等待原因，不包含外部响应或凭据。
// 参数：无。
// 返回值：固定业务等待码。
func (e *ImportTargetWaitError) Error() string { return e.Code }

var ErrImportTargetWaiting = errors.New("IMPORT_TARGET_WAITING")
var ErrImportTargetReceipt = errors.New("IMPORT_TARGET_RECEIPT_INVALID")

// Call 按专用机器能力访问历史协议，限制响应并禁止重定向。
// 参数：ctx为上下文，method/path为固定协议路由，payload为持久化命令，out为精确响应DTO。
// 返回值：认证、网络、状态或无效信封错误；不将裸200视为成功。
func (t *ImportTargetTransport) Call(ctx context.Context, method, path string, payload []byte, out any) error {
	if t == nil || t.BaseURL == "" || t.TokenSource == nil {
		return ErrImportTargetWaiting
	}
	token, e := t.TokenSource(ctx)
	if e != nil {
		return ErrImportTargetWaiting
	}
	if strings.TrimSpace(token) == "" {
		return ErrImportTargetWaiting
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(t.BaseURL, "/")+path, bytes.NewReader(payload))
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 15 * time.Second}
	if t.Client != nil {
		*c = *t.Client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := c.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	data, e := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if e != nil {
		return &ImportTargetWaitError{Code: "IMPORT_TARGET_RESPONSE_INTERRUPTED"}
	}
	if len(data) > 4*1024*1024 {
		return ErrImportTargetReceipt
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Code == "IMPORT_TARGET_NOT_READY" {
			return &ImportTargetWaitError{Code: "IMPORT_TARGET_NOT_READY"}
		}
		if resp.StatusCode == 400 || resp.StatusCode == 409 || resp.StatusCode == 422 {
			if len(failure.Code) > 128 {
				failure.Code = ""
			}
			for _, char := range failure.Code {
				if !(char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '.' || char == '-') {
					failure.Code = ""
					break
				}
			}
			return &ImportTargetHTTPError{Status: resp.StatusCode, Code: failure.Code}
		}
		return ErrImportTargetWaiting
	}
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" || (envelope.Code != "" && envelope.Code != "OK") {
		return ErrImportTargetReceipt
	}
	if json.Unmarshal(envelope.Data, out) != nil {
		return ErrImportTargetReceipt
	}
	return nil
}
