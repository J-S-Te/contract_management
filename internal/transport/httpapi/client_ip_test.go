package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestClientIPUsesProxyPublicAddress(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 125.120.19.87")
	if got := requestClientIP(req); got != "125.120.19.87" {
		t.Fatalf("client IP=%q, want right-most public address", got)
	}
}

// AUD-IP-2026：办公网/VPN/回环都是合法操作地址，不能因"非公网"被丢弃。
func TestRequestClientIPKeepsPrivateAddresses(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "10.8.0.5, 172.18.0.17")
	if got := requestClientIP(req); got != "172.18.0.17" {
		t.Fatalf("client IP=%q, want right-most private address", got)
	}
}

func TestRequestIPSourceMarksTrustedProxyAndDirect(t *testing.T) {
	proxied := httptest.NewRequest(http.MethodGet, "/", nil)
	proxied.RemoteAddr = "172.18.0.12:8080"
	proxied.Header.Set("X-Real-IP", "10.8.0.5")
	if got := requestIPSource(proxied); got != "TRUSTED_PROXY" {
		t.Fatalf("source=%q, want TRUSTED_PROXY", got)
	}
	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "203.0.113.5:12345"
	if got := requestIPSource(direct); got != "DIRECT" {
		t.Fatalf("source=%q, want DIRECT", got)
	}
}

func TestRequestClientIPIgnoresSpoofedHeadersFromPublicPeer(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:12345"
	req.Header.Set("X-Real-IP", "8.8.8.8")
	req.Header.Set("X-Forwarded-For", "8.8.8.8, 1.2.3.4")
	if got := requestClientIP(req); got != "203.0.113.5" {
		t.Fatalf("client IP=%q, want direct public peer 203.0.113.5", got)
	}
}
