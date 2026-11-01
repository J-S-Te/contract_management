package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	central "github.com/j-s-te/contract-management/internal/sessiongovernance"
)

type centralContractClient struct {
	central.Client
	state      central.Context
	err        error
	activities int
}

// State 提供中央窗口供合同实际认证逻辑读取，不计真实活动。
// 参数：请求和服务器绑定秘密。
// 返回值：预设窗口或依赖错误。
func (c *centralContractClient) State(context.Context, string) (central.Context, error) {
	return c.state, c.err
}

// Activity 只记录显式活动调用。
// 参数：请求和服务器绑定秘密。
// 返回值：预设窗口或依赖错误。
func (c *centralContractClient) Activity(context.Context, string) (central.Context, error) {
	c.activities++
	return c.state, c.err
}

type centralContractStore struct {
	OIDCStore
	record  SessionRecord
	revoked bool
}

// FindSession 提供本地已加密绑定的会话记录。
// 参数：请求、Cookie摘要和时钟。
// 返回值：预设会话，无错误。
func (s *centralContractStore) FindSession(context.Context, []byte, time.Time) (SessionRecord, error) {
	return s.record, nil
}

// RevokeSession 记录当前行撤销，禁止测试实现隐式扩大为账号撤销。
// 参数：请求、当前Cookie摘要和时钟。
// 返回值：无错误。
func (s *centralContractStore) RevokeSession(context.Context, []byte, time.Time) error {
	s.revoked = true
	return nil
}

// TestContractCentralAuthorityPrecedesTokenRefresh 验证旧会话不能补造周期，暂时故障不注销。
// 参数：t为测试上下文。
// 返回值：无；中央失效后仍继续认证、旧绑定被接受或故障误注销时失败。
func TestContractCentralAuthorityPrecedesTokenRefresh(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := codec.encrypt("server-binding")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                string
		binding             []byte
		upstream, errorWant error
		revoked             bool
	}{
		{"legacy", nil, nil, ErrUnauthenticated, false},
		{"central expired", cipher, central.ErrInactive, ErrUnauthenticated, true},
		{"dependency outage", cipher, central.ErrUnavailable, ErrAuthorizationServiceUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &centralContractClient{err: test.upstream}
			store := &centralContractStore{record: SessionRecord{TenantID: "tenant", CentralBindingCiphertext: test.binding, SessionExpiresAt: now.Add(8 * time.Hour)}}
			auth := &OIDCAuthenticator{store: store, codec: codec, now: func() time.Time { return now }, options: OIDCOptions{TenantID: "tenant", SessionCookieName: "contract-session", CentralClient: client}}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil)
			request.AddCookie(&http.Cookie{Name: "contract-session", Value: "test-cookie"})
			_, err := auth.Authenticate(context.Background(), request)
			if !errors.Is(err, test.errorWant) || store.revoked != test.revoked {
				t.Fatalf("got err%v revoked%v", err, store.revoked)
			}
			if client.activities != 0 {
				t.Fatal("ordinary request counted as interaction")
			}
		})
	}
}
