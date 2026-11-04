package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCentralStartupRetainsEqualLocalDeadlines 验证实际构造器按模式处理8h/8h本地期限。
// 参数：t 为测试上下文，OIDC发现只访问本用例回环服务，不连接数据库。
// 返回值：无；中央不能装配、旧模式放行或本地配置被重写时失败。
func TestCentralStartupRetainsEqualLocalDeadlines(t *testing.T) {
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize",
			"token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	defer issuer.Close()
	for _, enabled := range []bool{true, false} {
		options := OIDCOptions{
			CentralSessionEnabled: enabled, CentralSessionPublicBaseURL: "https://app.example.test",
			Issuer: issuer.URL, PlatformBaseURL: issuer.URL, ClientID: "startup-client",
			RedirectURI:    "https://app.example.test/contract_management/auth/callback",
			SessionIdleTTL: 8 * time.Hour, SessionTTL: 8 * time.Hour, AuthorizationTimeout: time.Second,
			Store: &GORMOIDCStore{}, SessionEncryptionKey: make([]byte, 32),
		}
		auth, err := NewOIDCAuthenticator(context.Background(), options)
		if !enabled {
			if err == nil || !strings.Contains(err.Error(), "idle TTL") {
				t.Fatalf("legacy equal deadline error=%v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("central constructor rejected controlled local deadlines: %v", err)
		}
		if auth.options.CentralClient == nil || auth.options.SessionIdleTTL != 8*time.Hour ||
			auth.options.SessionTTL != 8*time.Hour {
			t.Fatal("central mode or original local options were lost")
		}
	}
}
