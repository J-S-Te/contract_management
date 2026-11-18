package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	central "github.com/j-s-te/contract-management/internal/sessiongovernance"
)

type pageRefreshLockedStore struct{ idleSessionStore }

// AttachSessionLink 实现中央测试所需接口，不用于登录初始化。
// 参数：无业务输入，均为本地测试材料。
// 返回值：空事务和未认证错误。
func (s *pageRefreshLockedStore) AttachSessionLink(context.Context, []byte, []byte, time.Time) (LoginTransactionRecord, error) {
	return LoginTransactionRecord{}, ErrUnauthenticated
}

// WithSessionLock 将刷新回调作用于测试存储中的最新记录。
// 参数：update 为生产认证器执行的锁内复核逻辑，其余为本地测试参数。
// 返回值：已更新记录或原始错误，不代替真实 MySQL 并发验收。
func (s *pageRefreshLockedStore) WithSessionLock(_ context.Context, _ []byte, _ time.Time, update sessionUpdateFunc) (SessionRecord, error) {
	updated, changed, err := update(s.session)
	if err != nil {
		return SessionRecord{}, err
	}
	if changed {
		s.session = updated
	}
	return s.session, nil
}

// TestPageRefreshRechecksAuthorization 验证合同显式刷新绕过锁外及锁内缓存且无陈旧成功兜底。
// 参数：t 为生产认证器和临时 HTTP 平台的测试上下文。
// 返回值：无；标记丢失、错误分类或固定期限变化时失败。
func TestPageRefreshRechecksAuthorization(t *testing.T) {
	for _, centralMode := range []bool{false, true} {
		for _, tc := range []struct {
			name, path, marker string
			status             int
			stale              bool
			calls              int
		}{
			{"current roles", "/api/v1/auth/me", "1", 200, false, 1},
			{"removed", "/api/v1/auth/me", "1", 403, false, 1},
			{"outage", "/api/v1/auth/me", "1", 503, false, 1},
			{"stale outage", "/api/v1/auth/me", "1", 503, true, 1},
			{"ordinary cached read", "/api/v1/contracts", "", 200, false, 0},
			{"business marker ignored", "/api/v1/contracts", "1", 200, false, 0},
		} {
			t.Run(tc.name+map[bool]string{true: " central", false: " legacy"}[centralMode], func(t *testing.T) {
				now := idleTestNow
				store := &pageRefreshLockedStore{idleSessionStore: idleSessionStore{session: idleTestSession(now)}}
				auth := newIdleTestAuthenticator(t, store, now)
				auth.options.ClientID = "contract_management-prod-web"
				auth.options.ApplicationCode = "contract_management"
				auth.options.EnvironmentCode = "prod"
				auth.catalog = testCatalog()
				if tc.stale {
					store.session.AuthorizationCheckedAt = now.Add(-90 * time.Second)
				}
				store.session.AccessTokenCiphertext, _ = auth.codec.encrypt("valid-access-token")
				store.session.RefreshTokenCiphertext, _ = auth.codec.encrypt("refresh-token")
				store.session.IDTokenCiphertext, _ = auth.codec.encrypt("id-token")
				if centralMode {
					store.session.CentralBindingCiphertext, _ = auth.codec.encrypt("central-binding")
					store.session.CentralSessionID = "central-cycle"
					auth.options.CentralClient = &centralContractClient{state: central.Context{SessionID: "central-cycle", TenantID: "tenant-1", SubjectID: "identity-1", ClientID: auth.options.ClientID, ApplicationCode: "contract_management", EnvironmentCode: "prod", PolicyVersion: 1, IdleTimeoutSeconds: 1800, IdleRemainingMS: (30 * time.Minute).Milliseconds(), AbsoluteRemainingMS: (8 * time.Hour).Milliseconds(), AbsoluteExpiresAt: store.session.SessionExpiresAt}}
				}
				original := store.session
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Header.Get("X-Authorization-Refresh") != "1" {
						t.Error("freshness marker missing at platform")
					}
					w.WriteHeader(tc.status)
					if tc.status == 200 {
						_ = json.NewEncoder(w).Encode(AuthorizationContext{Subject: "kc-user-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: auth.options.ClientID, ApplicationCode: "contract_management", EnvironmentCode: "prod", Roles: []string{"sales"}, Permissions: []string{"contract.read"}, DataScopes: []AuthorizationDataScope{{RoleCode: "sales", ScopeType: "SELF", ScopeID: "identity-1"}}, AuthorizationRevision: 9})
					}
				}))
				defer server.Close()
				auth.authorization, _ = NewHTTPAuthorizationContextClient(server.URL, time.Second, server.Client().Transport)
				request := httptest.NewRequest("GET", tc.path, nil)
				request.Header.Set("X-Authorization-Refresh", tc.marker)
				request.AddCookie(&http.Cookie{Name: auth.options.SessionCookieName, Value: "raw-session"})
				principal, err := auth.Authenticate(context.Background(), request)
				var want error
				if tc.status == 403 {
					want = ErrAuthorizationForbidden
				}
				if tc.status == 503 {
					want = ErrAuthorizationServiceUnavailable
				}
				if calls != tc.calls || !errors.Is(err, want) {
					t.Fatalf("calls=%d want=%d err=%v want=%v", calls, tc.calls, err, want)
				}
				if tc.status == 200 && tc.calls == 1 {
					scope, ok := principal.Scope("contract.read")
					if principal.AuthorizationRevision != 9 || !ok || scope.AllowAll || !scope.AllowSelf || store.session.AuthorizationRevision != 9 {
						t.Fatalf("new permissions not applied: %+v scope=%+v", principal, scope)
					}
				}
				if !store.session.SessionExpiresAt.Equal(original.SessionExpiresAt) || !store.session.LastInteractiveAt.Equal(*original.LastInteractiveAt) {
					t.Fatal("refresh changed original deadlines")
				}
				if tc.status == 503 && store.revoked {
					t.Fatal("outage revoked login")
				}
			})
		}
	}
}
