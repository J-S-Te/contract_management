package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 状态污染风险：ConfigureTrustedProxies 是进程级配置，测试结束要还原默认值。
func withTrustedProxies(t *testing.T, values ...string) {
	t.Helper()
	previous := currentTrustedProxies()
	t.Cleanup(func() { trustedProxies.Store(previous) })
	ConfigureTrustedProxies(values)
}

// 只有显式列入可信代理的对端才允许改写来源地址。
func TestResolveClientIPUsesProxyForTrustedPeer(t *testing.T) {
	withTrustedProxies(t, "172.18.0.12/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "198.51.100.99, 125.120.19.87")
	if ip, source := resolveClientIP(req); ip != "125.120.19.87" || source != "TRUSTED_PROXY" {
		t.Fatalf("resolved=(%q,%q), want right-most client via trusted proxy", ip, source)
	}
}

// 私有网段不因地址段被信任：未列入列表时仍取连接对端。
func TestResolveClientIPKeepsPrivateAddressesButNotTrust(t *testing.T) {
	withTrustedProxies(t, "127.0.0.1/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "10.8.0.5, 172.18.0.17")
	if ip, source := resolveClientIP(req); ip != "172.18.0.12" || source != "DIRECT" {
		t.Fatalf("resolved=(%q,%q), want peer with DIRECT", ip, source)
	}
}

// 可信代理时，私有客户端地址仍要保留。
func TestResolveClientIPKeepsPrivateClientBehindProxy(t *testing.T) {
	withTrustedProxies(t, "172.18.0.12/32")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.12:8080"
	req.Header.Set("X-Forwarded-For", "10.8.0.5, 172.18.0.17")
	if ip, source := resolveClientIP(req); ip != "172.18.0.17" || source != "TRUSTED_PROXY" {
		t.Fatalf("resolved=(%q,%q), want private client preserved", ip, source)
	}
}

// 非可信对端伪造代理头必须被忽略；可信代理无转发头时也是直连观察。
func TestResolveClientIPIgnoresSpoofingAndFallsBack(t *testing.T) {
	withTrustedProxies(t, "192.0.2.10/32")
	spoofed := httptest.NewRequest(http.MethodGet, "/", nil)
	spoofed.RemoteAddr = "203.0.113.5:12345"
	spoofed.Header.Set("X-Real-IP", "8.8.8.8")
	spoofed.Header.Set("X-Forwarded-For", "8.8.8.8, 1.2.3.4")
	if ip, source := resolveClientIP(spoofed); ip != "203.0.113.5" || source != "DIRECT" {
		t.Fatalf("spoofed resolved=(%q,%q)", ip, source)
	}
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	plain.RemoteAddr = "192.0.2.10:5000"
	if ip, source := resolveClientIP(plain); ip != "192.0.2.10" || source != "DIRECT" {
		t.Fatalf("plain resolved=(%q,%q)", ip, source)
	}
}
