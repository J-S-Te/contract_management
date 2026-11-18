package platform

import (
	"context"
	"errors"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
	"github.com/j-s-te/contract-management/internal/authorizationrefresh"
)

type legacySessionLocker interface {
	WithSessionLock(context.Context, []byte, time.Time, sessionUpdateFunc) (SessionRecord, error)
}

// authenticateLegacyRefresh 在原会话行锁内重读并复核，隔离普通旧响应与页面新授权。
// 参数：ctx为请求，record定位当前Cookie，now为初始时钟，method控制既有只读降级。
// 返回值：当前合同主体；认证401、授权403、依赖503，不使用无锁回退。
func (a *OIDCAuthenticator) authenticateLegacyRefresh(ctx context.Context, record SessionRecord,
	now time.Time, method string) (application.Principal, error) {
	locker, ok := a.store.(legacySessionLocker)
	if !ok {
		return application.Principal{}, ErrAuthorizationServiceUnavailable
	}
	var principal application.Principal
	_, err := locker.WithSessionLock(ctx, record.SessionIDHash, now,
		func(current SessionRecord) (SessionRecord, bool, error) {
			lockedNow := a.now().UTC()
			var err error
			principal, err = principalFromJSON(current.PrincipalJSON)
			if err != nil || current.TenantID != a.options.TenantID || principal.TenantID != current.TenantID ||
				principal.IdentityID != current.IdentityID || !current.SessionExpiresAt.After(lockedNow) ||
				!a.sessionIdleValid(current, lockedNow) {
				return current, false, ErrUnauthenticated
			}
			forced := authorizationrefresh.Forced(ctx)
			if !forced && current.TokenExpiresAt.After(lockedNow) &&
				lockedNow.Sub(current.AuthorizationCheckedAt) < a.options.AuthorizationRefreshInterval {
				return current, false, nil
			}
			updated, changed, refreshErr := a.refreshAuthorization(ctx, current, principal, lockedNow)
			if refreshErr != nil {
				if !forced && errors.Is(refreshErr, ErrAuthorizationUnavailable) && safeStaleMethod(method) &&
					lockedNow.Sub(current.AuthorizationCheckedAt) <= a.options.AuthorizationMaxStale && current.TokenExpiresAt.After(lockedNow) {
					return current, false, nil
				}
				return current, false, refreshErr
			}
			if changed {
				principal, err = principalFromJSON(updated.PrincipalJSON)
				if err != nil {
					return current, false, ErrAuthorizationInvalid
				}
			}
			return updated, changed, nil
		})
	if errors.Is(err, ErrAuthorizationForbidden) || errors.Is(err, ErrAuthorizationInvalid) {
		_ = a.store.RevokeSessionsForIdentity(ctx, record.TenantID, record.IdentityID, a.now().UTC())
		return application.Principal{}, err
	}
	if errors.Is(err, ErrAuthorizationUnauthorized) || refreshTokenWasRejected(err) {
		_ = a.store.RevokeSessionsForIdentity(ctx, record.TenantID, record.IdentityID, a.now().UTC())
		return application.Principal{}, ErrUnauthenticated
	}
	if errors.Is(err, ErrUnauthenticated) {
		return application.Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return application.Principal{}, ErrAuthorizationServiceUnavailable
	}
	return principal, nil
}
