package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestImportTargetResponseInterruptedRetriesSameCommand 验证响应读取中断保留未知结果及原命令。
// 参数：t为测试上下文，使用真实本地HTTP连接模拟提交后截断正文。
// 返回值：无；永久阻塞、载荷变化或远端重复应用时失败。
func TestImportTargetResponseInterruptedRetriesSameCommand(t *testing.T) {
	var lock sync.Mutex
	requests, applications := 0, 0
	seen := map[string]bool{}
	payloads := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var command struct {
			CommandID string `json:"command_id"`
		}
		if err = json.Unmarshal(raw, &command); err != nil {
			t.Error(err)
			return
		}
		lock.Lock()
		requests++
		first := requests == 1
		payloads = append(payloads, string(raw))
		if !seen[command.CommandID] {
			seen[command.CommandID] = true
			applications++
		}
		lock.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"code":"OK","data":`)
			return
		}
		_, _ = io.WriteString(w, `{"code":"OK","data":{"receipt_id":"one-stable-result"}}`)
	}))
	defer server.Close()
	transport := &ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "isolated-machine-token", nil }, Client: server.Client()}
	frozen := []byte(`{"command_id":"frozen-command","source_revision":1}`)
	var result struct {
		ReceiptID string `json:"receipt_id"`
	}
	err := transport.Call(context.Background(), http.MethodPost, "/commands", frozen, &result)
	var wait *ImportTargetWaitError
	if errors.Is(err, ErrImportTargetReceipt) || !errors.As(err, &wait) || wait.Code != "IMPORT_TARGET_RESPONSE_INTERRUPTED" {
		t.Fatalf("interrupted response became a permanent receipt failure: %v", err)
	}
	if err = transport.Call(context.Background(), http.MethodPost, "/commands", frozen, &result); err != nil {
		t.Fatal(err)
	}
	lock.Lock()
	defer lock.Unlock()
	if applications != 1 || requests != 2 || len(payloads) != 2 || payloads[0] != payloads[1] || result.ReceiptID != "one-stable-result" {
		t.Fatalf("unsafe replay: applications=%d requests=%d", applications, requests)
	}
}

// TestImportTargetOversizedResponseRemainsProtocolError 验证完整但过大的响应不能作为回执。
// 参数：t为测试上下文，使用受控HTTP响应生成器。
// 返回值：无；超限回执被当成功或未知结果时失败。
func TestImportTargetOversizedResponseRemainsProtocolError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 4*1024*1024+1))
	}))
	defer server.Close()
	transport := &ImportTargetTransport{BaseURL: server.URL, TokenSource: func(context.Context) (string, error) { return "isolated-machine-token", nil }, Client: server.Client()}
	var result any
	if err := transport.Call(context.Background(), http.MethodGet, "/commands", nil, &result); !errors.Is(err, ErrImportTargetReceipt) {
		t.Fatalf("oversized response=%v", err)
	}
}
