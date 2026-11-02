package settlement

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestPermanentDeliveryFailureClassifies4xx 验证死信分类只覆盖 4xx，
// 且 408/429 与 5xx/网络错误保持可重试（BIZ-XEVT2-05 第 4 点）。
func TestPermanentDeliveryFailureClassifies4xx(t *testing.T) {
	cases := map[int]bool{
		400: true, 401: true, 403: true, 404: true, 409: true, 410: true, 422: true, 499: true,
		408: false, 429: false, 500: false, 502: false, 503: false, 504: false,
		200: false, 202: false, 302: false,
	}
	for status, want := range cases {
		if got := permanentDeliveryFailure(status); got != want {
			t.Fatalf("permanentDeliveryFailure(%d)=%v want %v", status, got, want)
		}
	}
}

// TestFinancialDispatcherMarksDeterministic4xxDeadOnFirstAttempt 验证结算侧确定
// 拒绝（409）在一次尝试内即判死信，不再空转 MaxAttempts 次。
func TestFinancialDispatcherMarksDeterministic4xxDeadOnFirstAttempt(t *testing.T) {
	store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(`{"event_type":"contract.financial_effective.v1"}`), Attempts: 1}}
	client := &http.Client{Transport: financialRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusConflict, Body: io.NopCloser(strings.NewReader("contract already has a financial event")), Header: make(http.Header)}, nil
	})}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 20, Client: client, TokenSource: func(context.Context) (string, error) { return "test-only-machine-token", nil }}
	if err := d.dispatchOne(context.Background()); err == nil {
		t.Fatal("确定性 4xx 必须返回投递错误")
	}
	if store.failed != "event" || !store.dead || store.delivered != "" {
		t.Fatalf("确定性 4xx 未在首次尝试判死信：failed=%q dead=%v delivered=%q", store.failed, store.dead, store.delivered)
	}
}

// TestFinancialDispatcherRetriesRetryableFailures 验证 408/429、5xx 与网络错误
// 仍按既有退避重试，不被误判为永久死信。
func TestFinancialDispatcherRetriesRetryableFailures(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(`{}`), Attempts: 1}}
		client := &http.Client{Transport: financialRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("temporary")), Header: make(http.Header)}, nil
		})}
		d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 20, Client: client}
		if err := d.dispatchOne(context.Background()); err == nil {
			t.Fatalf("status %d 必须返回投递错误", status)
		}
		if store.dead {
			t.Fatalf("status %d 被误判为永久死信", status)
		}
		if store.failed != "event" {
			t.Fatalf("status %d 未持久化重试状态", status)
		}
	}
	// 网络错误（无 HTTP 响应）同样可重试。
	store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(`{}`), Attempts: 1}}
	client := &http.Client{Transport: financialRoundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 20, Client: client}
	if err := d.dispatchOne(context.Background()); err == nil || store.dead {
		t.Fatalf("网络错误必须可重试：err=%v dead=%v", err, store.dead)
	}
}
