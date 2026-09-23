package platform

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 修复：回调失败时不能再 http.Error(... http.StatusText(status) ...)，用户会被卡在 plain text "Unauthorized/Forbidden"。
// 必须 302 跳到平台顶层 /access-error，前端 SubsystemAccessErrorView 才能渲染友好错误页 + 重试/重新登录/回门户按钮。
func TestWriteCallbackErrorRedirectsToAccessErrorPage(t *testing.T) {
	t.Run("redirects to platform access-error with stage/code/from/request_id", func(t *testing.T) {
		authenticator := &OIDCAuthenticator{
			options: OIDCOptions{
				PathPrefix: "/contract_management",
			},
		}
		request := httptest.NewRequest(http.MethodGet, "/contract_management/auth/callback?state=abc&code=xyz", nil)
		request.Header.Set("X-Request-ID", "req-callback-12345")
		recorder := httptest.NewRecorder()

		authenticator.writeCallbackError(recorder, request, "token_exchange", http.StatusUnauthorized, errOIDCRefreshTokenIsMissing)

		if got, want := recorder.Code, http.StatusFound; got != want {
			t.Fatalf("status = %d, want %d", got, want)
		}
		location := recorder.Header().Get("Location")
		if location == "" {
			t.Fatalf("missing Location header; body=%q", recorder.Body.String())
		}
		// 必须指向平台顶层 /access-error，不能带 /contract_management 前缀
		// （带前缀会再次触发合同路由守卫 / 形成死循环）。
		if strings.HasPrefix(location, "/contract_management") {
			t.Fatalf("Location must not carry subsystem prefix; got %q", location)
		}
		parsed, err := url.Parse(location)
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if parsed.Path != "/access-error" {
			t.Fatalf("Location path = %q, want /access-error", parsed.Path)
		}
		query := parsed.Query()
		if got := query.Get("reason"); got != "callback_failed" {
			t.Fatalf("reason = %q, want callback_failed", got)
		}
		if got := query.Get("stage"); got != "token_exchange" {
			t.Fatalf("stage = %q, want token_exchange", got)
		}
		if got := query.Get("code"); got != "401" {
			t.Fatalf("code = %q, want 401", got)
		}
		if got := query.Get("from"); got != "/contract_management/auth/callback" {
			t.Fatalf("from = %q, want /contract_management/auth/callback (no query string)", got)
		}
		if got := query.Get("request_id"); got != "req-callback-12345" {
			t.Fatalf("request_id = %q, want req-callback-12345", got)
		}
		// 不再返回 plain text "Unauthorized"：http.Redirect 会自动写入一个 <a href> 跳转提示，
		// 但浏览器在 30x 时不会渲染它（直接跟随 Location）。这里只验证 Content-Type
		// 不是 text/plain 的 plain-text 错误。
		if ct := recorder.Header().Get("Content-Type"); ct != "" && strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("Content-Type = %q, must not be text/plain", ct)
		}
	})

	t.Run("forbidden status still produces 302 with code=403", func(t *testing.T) {
		authenticator := &OIDCAuthenticator{options: OIDCOptions{PathPrefix: "/contract_management"}}
		request := httptest.NewRequest(http.MethodGet, "/contract_management/auth/callback", nil)
		recorder := httptest.NewRecorder()

		authenticator.writeCallbackError(recorder, request, "authorization_context", http.StatusForbidden, ErrAuthorizationForbidden)

		if recorder.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", recorder.Code)
		}
		parsed, err := url.Parse(recorder.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if got := parsed.Query().Get("code"); got != "403" {
			t.Fatalf("code = %q, want 403", got)
		}
	})

	t.Run("omits request_id when X-Request-ID header is missing", func(t *testing.T) {
		authenticator := &OIDCAuthenticator{options: OIDCOptions{PathPrefix: "/contract_management"}}
		request := httptest.NewRequest(http.MethodGet, "/contract_management/auth/callback", nil)
		recorder := httptest.NewRecorder()

		authenticator.writeCallbackError(recorder, request, "login_state", http.StatusUnauthorized, nil)

		parsed, err := url.Parse(recorder.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		if got := parsed.Query().Get("request_id"); got != "" {
			t.Fatalf("request_id = %q, want empty", got)
		}
	})
}
