package platform

import (
	"context"
	"errors"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// centralAuthenticationError 将明确拒绝与暂时故障分开，不因网络异常注销当前会话。
// 参数：err为中央校验、令牌或权限查询返回的错误。
// 返回值：确定失效为未认证，其余错误为暂时不可用。
func centralAuthenticationError(err error) error {
	if errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrAuthorizationForbidden) ||
		errors.Is(err, ErrAuthorizationUnauthorized) || errors.Is(err, ErrAuthorizationInvalid) || refreshTokenWasRejected(err) {
		return ErrUnauthenticated
	}
	return ErrAuthorizationServiceUnavailable
}

// rotateCentralTokens 在第一事务中重读、刷新并保存已验签的新凭据。
// 参数：ctx为请求，record为锁内最新记录，now为服务端时钟。
// 返回值：待提交记录和变更标记；不查询业务权限，不延长原会话截止。
func (a *OIDCAuthenticator) rotateCentralTokens(ctx context.Context, record SessionRecord, now time.Time) (SessionRecord, bool, error) {
	if _, err := a.centralState(ctx, record, false, false); err != nil {
		return record, false, err
	}
	if record.TokenExpiresAt.After(now.Add(time.Minute)) {
		return record, false, nil
	}
	current, err := principalFromJSON(record.PrincipalJSON)
	if err != nil {
		return record, false, ErrUnauthenticated
	}
	refresh, err := a.codec.decrypt(record.RefreshTokenCiphertext)
	if err != nil {
		return record, false, ErrUnauthenticated
	}
	token, err := a.refreshTokens(oidc.ClientContext(ctx, a.httpClient), &oauth2.Token{RefreshToken: refresh})
	if err != nil {
		return record, false, err
	}
	identity, rawID, expiry, err := a.verifyIDToken(ctx, token, "")
	if err != nil {
		return record, false, err
	}
	if identity.Subject != current.Subject || identity.IdentityID != record.IdentityID || identity.TenantID != record.TenantID ||
		record.OIDCSessionID == "" || identity.SessionID != record.OIDCSessionID {
		return record, false, ErrUnauthenticated
	}
	accessCipher, err := a.codec.encrypt(token.AccessToken)
	if err != nil {
		return record, false, err
	}
	refreshCipher, err := a.codec.encrypt(token.RefreshToken)
	if err != nil {
		return record, false, err
	}
	idCipher, err := a.codec.encrypt(rawID)
	if err != nil {
		return record, false, err
	}
	record.AccessTokenCiphertext, record.RefreshTokenCiphertext, record.IDTokenCiphertext = accessCipher, refreshCipher, idCipher
	record.TokenExpiresAt = earliestExpiry(expiry, token.Expiry)
	// 权限仍待第二事务验证；任何并发请求都不能把新访问令牌当作新权限证明。
	record.AuthorizationCheckedAt = time.Unix(1, 0).UTC()
	record.UpdatedAt = now
	return record, true, nil
}
