package platform

import (
	"context"
	"errors"
	"testing"

	central "github.com/j-s-te/contract-management/internal/sessiongovernance"
)

type importUnavailableCentral struct {
	central.Client
	calls int
}

// State 在导入复核中模拟中央会话依赖暂不可用。
// 参数：请求及绑定来自服务器端已有会话。
// 返回值：依赖错误，不能继续使用旧业务授权。
func (c *importUnavailableCentral) State(context.Context, string) (central.Context, error) {
	c.calls++
	return central.Context{}, central.ErrUnavailable
}

// TestResolveImportActorRetainsCentralAuthorityAndFrozenIdentity 验证导入不能绕过中央期限或冻结操作者。
// 参数：t为现有加密会话、中央依赖故障与错误操作者测试上下文。
// 返回值：无；跳过中央复核、错误放行或误撤销登录时失败。
func TestResolveImportActorRetainsCentralAuthorityAndFrozenIdentity(t *testing.T) {
	store := &pageRefreshLockedStore{idleSessionStore: idleSessionStore{session: idleTestSession(idleTestNow)}}
	auth := newIdleTestAuthenticator(t, store, idleTestNow)
	client := &importUnavailableCentral{}
	auth.options.CentralClient = client
	store.session.CentralBindingCiphertext, _ = auth.codec.encrypt("synthetic-binding")
	if _, err := auth.ResolveImportActor(context.Background(), store.session.SessionIDHash, "tenant-1", "identity-1"); !errors.Is(err, ErrAuthorizationServiceUnavailable) || client.calls != 1 || store.revoked {
		t.Fatalf("central boundary lost: calls=%d revoked=%t error=%v", client.calls, store.revoked, err)
	}
	if _, err := auth.ResolveImportActor(context.Background(), store.session.SessionIDHash, "tenant-1", "other-actor"); !errors.Is(err, ErrUnauthenticated) || client.calls != 1 {
		t.Fatalf("frozen actor was not enforced: calls=%d error=%v", client.calls, err)
	}
}
