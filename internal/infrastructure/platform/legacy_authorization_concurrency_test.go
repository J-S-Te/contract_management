package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type legacyConcurrentKey struct{}
type legacyConcurrentStore struct {
	*idleSessionStore
	stateMu, rowMu sync.Mutex
	secondLock     chan struct{}
	lockSignal     sync.Once
	oldHolding     atomic.Bool
}

// FindSession 为并发认证返回独立的最新会话快照。
// 参数：ctx为请求，hash为Cookie摘要，now为有效性时钟。
// 返回值：内存存储快照或失效错误，不访问外部数据库。
func (s *legacyConcurrentStore) FindSession(ctx context.Context, hash []byte, now time.Time) (SessionRecord, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.idleSessionStore.FindSession(ctx, hash, now)
}

// UpdateSession 原子提交真实认证器产生的会话快照。
// 参数：ctx为请求，record为已复核快照。
// 返回值：内存写回结果。
func (s *legacyConcurrentStore) UpdateSession(ctx context.Context, record SessionRecord) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.idleSessionStore.UpdateSession(ctx, record)
}

// TouchSessionLastSeen 串行更新观测时间，不修改授权快照。
// 参数：ctx为请求，hash为Cookie摘要，now为观测时钟。
// 返回值：内存更新时间的结果。
func (s *legacyConcurrentStore) TouchSessionLastSeen(ctx context.Context, hash []byte, now time.Time) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.idleSessionStore.TouchSessionLastSeen(ctx, hash, now)
}

// WithSessionLock 在会话行锁内重读、复核并提交，所有授权写回共享同一边界。
// 参数：ctx为请求，hash和now限定会话，update为生产认证器的授权复核操作。
// 返回值：最新提交快照；错误不提交回调结果。
func (s *legacyConcurrentStore) WithSessionLock(ctx context.Context, hash []byte, now time.Time, update sessionUpdateFunc) (SessionRecord, error) {
	if ctx.Value(legacyConcurrentKey{}) == "fresh" {
		s.lockSignal.Do(func() { close(s.secondLock) })
	}
	s.rowMu.Lock()
	defer s.rowMu.Unlock()
	if ctx.Value(legacyConcurrentKey{}) != "fresh" {
		s.oldHolding.Store(true)
		defer s.oldHolding.Store(false)
	}
	current, err := s.FindSession(ctx, hash, now)
	if err != nil {
		return current, err
	}
	updated, changed, err := update(current)
	if err != nil {
		return current, err
	}
	if changed {
		if err := s.UpdateSession(ctx, updated); err != nil {
			return current, err
		}
	}
	return s.FindSession(ctx, hash, now)
}

// TestLegacyConcurrentAuthorizationNeverRestoresWideScope 验证普通旧读不能迟到覆盖同revision的新窄授权。
// 参数：t为真实认证器、并发内存行锁与同一Cookie测试上下文。
// 返回值：无；最终普通请求恢复宽权限、增加远程读取或延长期限时失败。
func TestLegacyConcurrentAuthorizationNeverRestoresWideScope(t *testing.T) {
	for _, importActor := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary authentication", true: "background import actor"}[importActor], func(t *testing.T) {

			now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
			record := idleTestSession(now)
			record.AuthorizationCheckedAt = now.Add(-2 * time.Minute)
			record.AuthorizationRevision = 9
			record.SessionIDHash = tokenHash("same-cookie")
			store := &legacyConcurrentStore{idleSessionStore: &idleSessionStore{session: record}, secondLock: make(chan struct{})}
			oldStarted, releaseOld, freshStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			auth := newIdleTestAuthenticator(t, store, now)
			auth.options.SessionCookieName = "legacy_session"
			auth.options.ClientID = "contract_management-prod-web"
			auth.options.ApplicationCode = "contract_management"
			auth.options.EnvironmentCode = "prod"
			auth.catalog = testCatalog()
			store.session.AccessTokenCiphertext, _ = auth.codec.encrypt("valid-access")
			store.session.RefreshTokenCiphertext, _ = auth.codec.encrypt("refresh")
			store.session.IDTokenCiphertext, _ = auth.codec.encrypt("id")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if importActor && r.Header.Get("X-Authorization-Refresh") != "1" {
					t.Error("import authority read lost freshness marker")
				}
				scope := AuthorizationDataScope{RoleCode: "sales", ScopeType: "APPLICATION"}
				if call > 1 {
					close(freshStarted)
					scope = AuthorizationDataScope{RoleCode: "sales", ScopeType: "SELF", ScopeID: "identity-1"}
				} else {
					close(oldStarted)
					<-releaseOld
				}
				_ = json.NewEncoder(w).Encode(AuthorizationContext{Subject: "kc-user-1", IdentityID: "identity-1", TenantID: "tenant-1", ClientID: auth.options.ClientID, ApplicationCode: "contract_management", EnvironmentCode: "prod", Roles: []string{"sales"}, Permissions: []string{"contract.read"}, DataScopes: []AuthorizationDataScope{scope}, AuthorizationRevision: 9})
			}))
			defer server.Close()
			auth.authorization, _ = NewHTTPAuthorizationContextClient(server.URL, time.Second, server.Client().Transport)
			request := func(fresh bool) *http.Request {
				path := "/api/v1/contracts"
				if fresh {
					path = "/api/v1/auth/me"
				}
				r := httptest.NewRequest("GET", path, nil)
				r.AddCookie(&http.Cookie{Name: "legacy_session", Value: "same-cookie"})
				if fresh {
					r.Header.Set("X-Authorization-Refresh", "1")
				}
				return r
			}
			oldDone, freshDone := make(chan error, 1), make(chan error, 1)
			go func() {
				var err error
				if importActor {
					_, err = auth.ResolveImportActor(context.Background(), record.SessionIDHash, "tenant-1", "identity-1")
				} else {
					_, err = auth.Authenticate(context.Background(), request(false))
				}
				oldDone <- err
			}()
			select {
			case <-oldStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary authorization read did not start")
			}
			go func() {
				ctx := context.WithValue(context.Background(), legacyConcurrentKey{}, "fresh")
				_, err := auth.Authenticate(ctx, request(true))
				freshDone <- err
			}()
			oldHoldsLock := store.oldHolding.Load()
			freshFinished := false
			if oldHoldsLock {
				select {
				case <-store.secondLock:
				case <-time.After(5 * time.Second):
					t.Fatal("fresh read did not queue on session lock")
				}
			} else {
				select {
				case <-freshStarted:
				case <-time.After(5 * time.Second):
					t.Fatal("fresh source read did not start")
				}
				select {
				case err := <-freshDone:
					if err != nil {
						t.Fatal(err)
					}
					freshFinished = true
				case <-time.After(5 * time.Second):
					t.Fatal("fresh read did not finish")
				}
			}
			close(releaseOld)
			select {
			case err := <-oldDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ordinary read did not finish")
			}
			if !freshFinished {
				select {
				case err := <-freshDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("fresh read did not finish after lock")
				}
			}
			current, err := auth.Authenticate(context.Background(), request(false))
			scope, ok := current.Scope("contract.read")
			if err != nil || !ok || scope.AllowAll || !scope.AllowSelf || calls.Load() != 2 {
				t.Fatalf("late ordinary read restored authorization: principal=%+v scope=%+v calls=%d error=%v", current, scope, calls.Load(), err)
			}
			persisted, err := store.FindSession(context.Background(), record.SessionIDHash, now)
			if err != nil || !persisted.SessionExpiresAt.Equal(record.SessionExpiresAt) || !persisted.LastInteractiveAt.Equal(*record.LastInteractiveAt) {
				t.Fatal("authorization serialization changed original deadlines")
			}
		})
	}
}
