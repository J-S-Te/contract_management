package platform

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/authorizationrefresh"
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

// ResolveImportActor 在每项执行前串行读取冻结操作者的当前权威授权。
// 参数：ctx控制远程调用，hash为原会话引用，tenant和actor为任务冻结的可信身份。
// 返回值：当前主体或401、403、503语义；不使用陈旧权限回退或无锁写回。
func (a *OIDCAuthenticator) ResolveImportActor(ctx context.Context, hash []byte, tenant, actor string) (application.Principal, error) {
	if tenant != a.options.TenantID {
		return application.Principal{}, ErrUnauthenticated
	}
	ctx = authorizationrefresh.WithForce(ctx)
	now := a.now().UTC()
	row, err := a.store.FindSession(ctx, hash, now)
	if errors.Is(err, ErrUnauthenticated) {
		return application.Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return application.Principal{}, ErrAuthorizationServiceUnavailable
	}
	if row.TenantID != tenant || row.IdentityID != actor {
		return application.Principal{}, ErrUnauthenticated
	}
	var principal application.Principal
	if a.options.CentralClient != nil {
		principal, err = a.authenticateCentral(ctx, row, now)
	} else {
		principal, err = a.resolveLegacyImportActor(ctx, hash, tenant, actor, now)
	}
	if err != nil {
		return application.Principal{}, err
	}
	if principal.IdentityID != actor || principal.TenantID != tenant {
		return application.Principal{}, ErrUnauthenticated
	}
	return principal, nil
}

// resolveLegacyImportActor 在原会话行锁内验证冻结身份并复核导入授权。
// 参数：ctx已要求新鲜读取，hash定位会话，tenant和actor为冻结边界，now为初始时钟。
// 返回值：已提交的新主体或准确错误，保留导入原有绝对期限策略。
func (a *OIDCAuthenticator) resolveLegacyImportActor(ctx context.Context, hash []byte, tenant, actor string,
	now time.Time) (application.Principal, error) {
	locker, ok := a.store.(legacySessionLocker)
	if !ok {
		return application.Principal{}, ErrAuthorizationServiceUnavailable
	}
	updated, err := locker.WithSessionLock(ctx, hash, now, func(current SessionRecord) (SessionRecord, bool, error) {
		lockedNow := a.now().UTC()
		principal, err := principalFromJSON(current.PrincipalJSON)
		if err != nil || current.TenantID != tenant || current.IdentityID != actor || principal.TenantID != tenant ||
			principal.IdentityID != actor || !current.SessionExpiresAt.After(lockedNow) {
			return current, false, ErrUnauthenticated
		}
		return a.refreshAuthorization(ctx, current, principal, lockedNow)
	})
	if err != nil {
		return application.Principal{}, centralAuthenticationError(err)
	}
	return principalFromJSON(updated.PrincipalJSON)
}
