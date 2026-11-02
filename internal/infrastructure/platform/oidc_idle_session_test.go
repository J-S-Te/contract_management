package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"golang.org/x/oauth2"
)

// idleSessionStore 是 OIDCStore 的内存实现，只模拟本组用例需要的会话读写语义：
// 普通请求只推进 last_seen_at，显式活动才推进 last_interactive_at。
type idleSessionStore struct {
	session         SessionRecord
	findErr         error
	revoked         bool
	lastSeenTouches []time.Time
	interactions    []time.Time
}

func (s *idleSessionStore) SaveLoginTransaction(context.Context, LoginTransactionRecord) error {
	return nil
}

func (s *idleSessionStore) ConsumeLoginTransaction(context.Context, []byte, time.Time) (LoginTransactionRecord, error) {
	return LoginTransactionRecord{}, nil
}

func (s *idleSessionStore) CreateSession(_ context.Context, record SessionRecord) error {
	s.session = record
	return nil
}

func (s *idleSessionStore) FindSession(_ context.Context, _ []byte, now time.Time) (SessionRecord, error) {
	if s.findErr != nil {
		return SessionRecord{}, s.findErr
	}
	if s.revoked || !s.session.SessionExpiresAt.After(now) {
		return SessionRecord{}, ErrUnauthenticated
	}
	return s.session, nil
}

func (s *idleSessionStore) TouchSessionLastSeen(_ context.Context, _ []byte, now time.Time) error {
	if s.revoked || !s.session.SessionExpiresAt.After(now) {
		return ErrUnauthenticated
	}
	s.lastSeenTouches = append(s.lastSeenTouches, now)
	value := now
	s.session.LastSeenAt = &value
	return nil
}

func (s *idleSessionStore) RecordInteraction(_ context.Context, _ []byte, now time.Time, idleTimeout time.Duration) error {
	if idleTimeout <= 0 || s.revoked || !s.session.SessionExpiresAt.After(now) ||
		!now.Before(interactiveBaseline(s.session).Add(idleTimeout)) {
		return ErrUnauthenticated
	}
	s.interactions = append(s.interactions, now)
	value := now
	s.session.LastSeenAt = &value
	s.session.LastInteractiveAt = &value
	return nil
}

func (s *idleSessionStore) UpdateSession(_ context.Context, record SessionRecord) error {
	s.session = record
	return nil
}

func (s *idleSessionStore) RevokeSession(_ context.Context, _ []byte, _ time.Time) error {
	s.revoked = true
	return nil
}

func (s *idleSessionStore) RevokeSessionsForIdentity(context.Context, string, string, time.Time) error {
	s.revoked = true
	return nil
}

func (s *idleSessionStore) DeleteExpired(context.Context, time.Time) error { return nil }

var idleTestNow = time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)

func idleTestPrincipal() application.Principal {
	return application.Principal{
		Subject: "kc-user-1", TenantID: "tenant-1", UserID: "identity-1", IdentityID: "identity-1",
		AuthorizationRevision: 1,
	}
}

func idleTestSession(now time.Time) SessionRecord {
	principalJSON, err := json.Marshal(idleTestPrincipal())
	if err != nil {
		panic(err)
	}
	lastSeen := now
	return SessionRecord{
		SessionIDHash: tokenHash("raw-session"), TenantID: "tenant-1", IdentityID: "identity-1",
		PrincipalJSON: principalJSON, AuthorizationRevision: 1,
		AuthorizationCheckedAt: now, TokenExpiresAt: now.Add(time.Hour),
		SessionExpiresAt: now.Add(8 * time.Hour),
		LastSeenAt:       &lastSeen, LastInteractiveAt: &lastSeen,
		CreatedAt: now, UpdatedAt: now,
	}
}

func newIdleTestAuthenticator(t *testing.T, store OIDCStore, now time.Time) *OIDCAuthenticator {
	t.Helper()
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatalf("newSecretCodec() error = %v", err)
	}
	return &OIDCAuthenticator{
		options: OIDCOptions{
			TenantID: "tenant-1", SessionCookieName: "contract_management_session",
			SessionTTL: 8 * time.Hour, SessionIdleTTL: 30 * time.Minute,
			AuthorizationRefreshInterval: time.Minute, AuthorizationMaxStale: 2 * time.Minute,
		},
		store: store, codec: codec,
		now: func() time.Time { return now },
	}
}

func idleTestRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/contracts", nil)
	request.AddCookie(&http.Cookie{Name: "contract_management_session", Value: "raw-session"})
	return request
}

// TestNewOIDCAuthenticatorRejectsIdleTTLNotShorterThanAbsolute 验证启动期配置失败关闭：
// 空闲期限必须为正且严格短于绝对期限，否则空闲机制没有意义。
func TestNewOIDCAuthenticatorRejectsIdleTTLNotShorterThanAbsolute(t *testing.T) {
	for _, options := range []OIDCOptions{
		{Store: &idleSessionStore{}, SessionTTL: 8 * time.Hour, SessionIdleTTL: 8 * time.Hour},
		{Store: &idleSessionStore{}, SessionTTL: 8 * time.Hour, SessionIdleTTL: 0},
		{Store: &idleSessionStore{}, SessionTTL: 0, SessionIdleTTL: 0},
	} {
		if _, err := NewOIDCAuthenticator(context.Background(), options); err == nil || !strings.Contains(err.Error(), "idle") {
			t.Fatalf("NewOIDCAuthenticator() error = %v, want idle TTL validation error", err)
		}
	}
}

// TestNewSessionRecordStartsIdleWindow 验证新建会话同时拥有绝对期限与空闲基准。
func TestNewSessionRecordStartsIdleWindow(t *testing.T) {
	store := &idleSessionStore{}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)
	record, err := authenticator.newSessionRecord("raw-session", idleTestPrincipal(),
		&oauth2.Token{AccessToken: "access-token", RefreshToken: "refresh-token"}, "raw-id-token",
		idleTestNow.Add(time.Hour), idleTestNow)
	if err != nil {
		t.Fatalf("newSessionRecord() error = %v", err)
	}
	if !record.SessionExpiresAt.Equal(idleTestNow.Add(8 * time.Hour)) {
		t.Fatalf("SessionExpiresAt = %v, want absolute 8h", record.SessionExpiresAt)
	}
	if record.LastInteractiveAt == nil || !record.LastInteractiveAt.Equal(idleTestNow) {
		t.Fatalf("LastInteractiveAt = %v, want creation time", record.LastInteractiveAt)
	}
	if record.LastSeenAt == nil || !record.LastSeenAt.Equal(idleTestNow) {
		t.Fatalf("LastSeenAt = %v, want creation time", record.LastSeenAt)
	}
}

// TestAuthenticateAllowsSessionWithinIdleWindowAndOnlyTouchesLastSeen 验证普通认证请求
// 只写 last_seen_at，不会顺带推进空闲基准（轮询不能续命）。
func TestAuthenticateAllowsSessionWithinIdleWindowAndOnlyTouchesLastSeen(t *testing.T) {
	session := idleTestSession(idleTestNow)
	lastInteractive := idleTestNow.Add(-29 * time.Minute)
	session.LastInteractiveAt = &lastInteractive
	store := &idleSessionStore{session: session}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

	principal, err := authenticator.Authenticate(context.Background(), idleTestRequest())
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if principal.IdentityID != "identity-1" {
		t.Fatalf("principal = %#v", principal)
	}
	if len(store.lastSeenTouches) != 1 {
		t.Fatalf("last_seen touches = %d, want 1", len(store.lastSeenTouches))
	}
	if len(store.interactions) != 0 {
		t.Fatalf("ordinary request recorded %d interactions, want 0", len(store.interactions))
	}
	if store.session.LastInteractiveAt == nil || !store.session.LastInteractiveAt.Equal(lastInteractive) {
		t.Fatalf("last_interactive_at changed by ordinary request: %v", store.session.LastInteractiveAt)
	}
}

// TestAuthenticateRejectsIdleExpiredSessionWithoutReviving 验证空闲到期后普通请求返回未认证，
// 且不会通过写 last_seen 或撤销操作"复活"会话。
func TestAuthenticateRejectsIdleExpiredSessionWithoutReviving(t *testing.T) {
	session := idleTestSession(idleTestNow)
	lastInteractive := idleTestNow.Add(-31 * time.Minute)
	session.LastInteractiveAt = &lastInteractive
	store := &idleSessionStore{session: session}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

	if _, err := authenticator.Authenticate(context.Background(), idleTestRequest()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
	}
	if len(store.lastSeenTouches) != 0 {
		t.Fatalf("idle-expired request touched last_seen_at %d times, want 0", len(store.lastSeenTouches))
	}
	if store.revoked {
		t.Fatal("idle expiry must not revoke the whole account session")
	}
}

// TestAuthenticateTreatsMissingInteractiveTimeAsCreatedAt 验证旧存量行（last_interactive_at=NULL）
// 按 created_at 计算空闲窗口，升级后既有会话不会因空值被误判。
func TestAuthenticateTreatsMissingInteractiveTimeAsCreatedAt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		created  time.Time
		wantAuth bool
	}{
		{"旧行仍在空闲窗口内", idleTestNow.Add(-10 * time.Minute), true},
		{"旧行已超过空闲期限", idleTestNow.Add(-31 * time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := idleTestSession(idleTestNow)
			session.LastInteractiveAt = nil
			session.CreatedAt = tc.created
			store := &idleSessionStore{session: session}
			authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

			_, err := authenticator.Authenticate(context.Background(), idleTestRequest())
			if tc.wantAuth && err != nil {
				t.Fatalf("Authenticate() error = %v, want success", err)
			}
			if !tc.wantAuth && !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

// TestAuthenticateRejectsAbsoluteExpiry 验证绝对到期优先于空闲窗口，活动不能救回绝对过期会话。
func TestAuthenticateRejectsAbsoluteExpiry(t *testing.T) {
	session := idleTestSession(idleTestNow)
	session.SessionExpiresAt = idleTestNow.Add(-time.Second)
	lastInteractive := idleTestNow
	session.LastInteractiveAt = &lastInteractive
	store := &idleSessionStore{session: session}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

	if _, err := authenticator.Authenticate(context.Background(), idleTestRequest()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("Authenticate() error = %v, want ErrUnauthenticated", err)
	}
}

// TestRecordActivityRefreshesIdleWindowButNotAbsoluteExpiry 验证显式活动只推进空闲基准，
// 绝对期限保持不变，并返回设计约定的空闲期限/剩余窗口。
func TestRecordActivityRefreshesIdleWindowButNotAbsoluteExpiry(t *testing.T) {
	session := idleTestSession(idleTestNow)
	lastInteractive := idleTestNow.Add(-20 * time.Minute)
	session.LastInteractiveAt = &lastInteractive
	absoluteBefore := session.SessionExpiresAt
	store := &idleSessionStore{session: session}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

	idleTimeout, remaining, err := authenticator.RecordActivity(context.Background(), idleTestRequest())
	if err != nil {
		t.Fatalf("RecordActivity() error = %v", err)
	}
	if idleTimeout != 30*time.Minute {
		t.Fatalf("idleTimeout = %v, want 30m", idleTimeout)
	}
	if remaining != 30*time.Minute {
		t.Fatalf("idleRemaining = %v, want full 30m after interaction", remaining)
	}
	if len(store.interactions) != 1 {
		t.Fatalf("interactions = %d, want 1", len(store.interactions))
	}
	if !store.session.SessionExpiresAt.Equal(absoluteBefore) {
		t.Fatalf("absolute expiry changed by activity: %v -> %v", absoluteBefore, store.session.SessionExpiresAt)
	}
	if store.session.LastInteractiveAt == nil || !store.session.LastInteractiveAt.Equal(idleTestNow) {
		t.Fatalf("last_interactive_at = %v, want now", store.session.LastInteractiveAt)
	}
}

// TestRecordActivityRejectsIdleExpiredSession 验证空闲到期后活动接口 401 且不会复活会话。
func TestRecordActivityRejectsIdleExpiredSession(t *testing.T) {
	session := idleTestSession(idleTestNow)
	lastInteractive := idleTestNow.Add(-31 * time.Minute)
	session.LastInteractiveAt = &lastInteractive
	store := &idleSessionStore{session: session}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

	if _, _, err := authenticator.RecordActivity(context.Background(), idleTestRequest()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("RecordActivity() error = %v, want ErrUnauthenticated", err)
	}
	if len(store.interactions) != 0 {
		t.Fatalf("idle-expired interaction was recorded %d times, want 0", len(store.interactions))
	}
}

// TestRecordActivityRejectsRevokedOrAbsoluteExpiredSession 验证已撤销/绝对到期会话不能被活动接口恢复。
func TestRecordActivityRejectsRevokedOrAbsoluteExpiredSession(t *testing.T) {
	t.Run("已撤销", func(t *testing.T) {
		store := &idleSessionStore{session: idleTestSession(idleTestNow), revoked: true}
		authenticator := newIdleTestAuthenticator(t, store, idleTestNow)
		if _, _, err := authenticator.RecordActivity(context.Background(), idleTestRequest()); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("RecordActivity() error = %v, want ErrUnauthenticated", err)
		}
	})
	t.Run("绝对到期", func(t *testing.T) {
		session := idleTestSession(idleTestNow)
		session.SessionExpiresAt = idleTestNow.Add(-time.Second)
		store := &idleSessionStore{session: session}
		authenticator := newIdleTestAuthenticator(t, store, idleTestNow)
		if _, _, err := authenticator.RecordActivity(context.Background(), idleTestRequest()); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("RecordActivity() error = %v, want ErrUnauthenticated", err)
		}
	})
}

// TestSessionIdleCapsRemainingByAbsoluteExpiry 验证 /auth/me 使用的剩余窗口取
// min(last_interactive+idle, absolute) - now，且不推进任何时间基准。
func TestSessionIdleCapsRemainingByAbsoluteExpiry(t *testing.T) {
	session := idleTestSession(idleTestNow)
	session.SessionExpiresAt = idleTestNow.Add(5 * time.Minute)
	store := &idleSessionStore{session: session}
	authenticator := newIdleTestAuthenticator(t, store, idleTestNow)

	idleTimeout, remaining, err := authenticator.SessionIdle(context.Background(), idleTestRequest())
	if err != nil {
		t.Fatalf("SessionIdle() error = %v", err)
	}
	if idleTimeout != 30*time.Minute {
		t.Fatalf("idleTimeout = %v, want 30m", idleTimeout)
	}
	if remaining != 5*time.Minute {
		t.Fatalf("idleRemaining = %v, want absolute-capped 5m", remaining)
	}
	if len(store.lastSeenTouches) != 0 || len(store.interactions) != 0 {
		t.Fatalf("SessionIdle must not touch session: seen=%d interactions=%d",
			len(store.lastSeenTouches), len(store.interactions))
	}
}
