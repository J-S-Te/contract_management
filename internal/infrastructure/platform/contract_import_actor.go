package platform

import (
	"context"
	"github.com/j-s-te/contract-management/internal/application"
	"net/http"
)

// ImportExecutionSession 返回现有会话的不可逆引用，不向台账复制令牌。
// 参数：request为已通过鉴权的当前请求。
// 返回值：会话哈希或未登录错误。
func (a *OIDCAuthenticator) ImportExecutionSession(request *http.Request) ([]byte, error) {
	cookie, err := request.Cookie(a.options.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, ErrUnauthenticated
	}
	return tokenHash(cookie.Value), nil
}

// ResolveImportActor 每项执行前从现有加密会话重新获取平台权威授权。
// 参数：ctx控制远程调用，hash为会话引用，tenant及actor为冻结的真实操作者。
// 返回值：当前身份或授权错误；不使用陈旧授权回退。
func (a *OIDCAuthenticator) ResolveImportActor(ctx context.Context, hash []byte, tenant, actor string) (application.Principal, error) {
	now := a.now().UTC()
	row, err := a.store.FindSession(ctx, hash, now)
	if err != nil || tenant != a.options.TenantID || row.TenantID != tenant || row.IdentityID != actor {
		return application.Principal{}, ErrUnauthenticated
	}
	current, err := principalFromJSON(row.PrincipalJSON)
	if err != nil || current.IdentityID != actor || current.TenantID != tenant {
		return application.Principal{}, ErrUnauthenticated
	}
	updated, _, err := a.refreshAuthorization(ctx, row, current, now)
	if err != nil {
		return application.Principal{}, err
	}
	if err = a.store.UpdateSession(ctx, updated); err != nil {
		return application.Principal{}, ErrAuthorizationServiceUnavailable
	}
	return principalFromJSON(updated.PrincipalJSON)
}
