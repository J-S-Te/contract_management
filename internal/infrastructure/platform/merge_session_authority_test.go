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

// TestLegacyIdleEndpointsRespectCentralAuthority 验证旧空闲接口也使用中央权威窗口。
// 参数：t 为测试上下文。
// 返回值：无；本地旧活动时间覆盖中央状态、后台续活或依赖错误分类异常时失败。
func TestLegacyIdleEndpointsRespectCentralAuthority(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	codec, err := newSecretCodec(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := codec.encrypt("binding")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name           string
		upstream, want error
	}{
		{"active", nil, nil},
		{"inactive", central.ErrInactive, ErrUnauthenticated},
		{"outage", central.ErrUnavailable, ErrAuthorizationServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &centralContractClient{err: test.upstream, state: central.Context{
				SessionID: "cycle", TenantID: "tenant", SubjectID: "identity", ClientID: "client",
				ApplicationCode: "app", EnvironmentCode: "dev", PolicyVersion: 1,
				IdleTimeoutSeconds: 1800, IdleRemainingMS: 120000,
				AbsoluteExpiresAt: now.Add(time.Hour), AbsoluteRemainingMS: 3600000,
			}}
			store := &centralContractStore{record: SessionRecord{
				TenantID: "tenant", IdentityID: "identity", CentralSessionID: "cycle", CentralBindingCiphertext: cipher,
				PrincipalJSON: []byte(`{"TenantID":"tenant","IdentityID":"identity","UserID":"identity","AuthorizationRevision":1}`),
				CreatedAt:     now.Add(-time.Hour), SessionExpiresAt: now.Add(time.Hour),
			}}
			auth := &OIDCAuthenticator{store: store, codec: codec, now: func() time.Time { return now }, options: OIDCOptions{
				TenantID: "tenant", ClientID: "client", ApplicationCode: "app", EnvironmentCode: "dev",
				SessionCookieName: "session", SessionIdleTTL: 30 * time.Minute, CentralClient: client,
			}}
			request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			request.AddCookie(&http.Cookie{Name: "session", Value: "cookie"})
			idle, remaining, err := auth.SessionIdle(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("read error=%v want=%v", err, test.want)
			}
			if err == nil && (idle != 30*time.Minute || remaining != 2*time.Minute) {
				t.Fatalf("idle=%v remaining=%v", idle, remaining)
			}
			if client.activities != 0 {
				t.Fatal("read counted as activity")
			}
			idle, remaining, err = auth.RecordActivity(context.Background(), request)
			if !errors.Is(err, test.want) {
				t.Fatalf("activity error=%v want=%v", err, test.want)
			}
			if err == nil && (idle != 30*time.Minute || remaining != 2*time.Minute) {
				t.Fatalf("activity idle=%v remaining=%v", idle, remaining)
			}
			if client.activities != 1 {
				t.Fatalf("activities=%d", client.activities)
			}
			if test.upstream == central.ErrUnavailable && store.revoked {
				t.Fatal("outage revoked session")
			}
		})
	}
}
