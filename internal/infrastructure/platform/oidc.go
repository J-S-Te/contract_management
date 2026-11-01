package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/j-s-te/contract-management/internal/application"
	central "github.com/j-s-te/contract-management/internal/sessiongovernance"
	"golang.org/x/oauth2"
)

const loginTransactionTTL = 10 * time.Minute

var (
	ErrUnauthenticated                 = errors.New("unauthenticated")
	ErrAuthorizationServiceUnavailable = errors.New("authorization service unavailable")
	errOIDCRefreshTokenIsMissing       = errors.New("OIDC refresh token is missing")
)

type OIDCOptions struct {
	CentralSessionEnabled                                           bool
	CentralSessionPublicBaseURL                                     string
	CentralClient                                                   central.Client
	Issuer, BackchannelBaseURL, PlatformBaseURL                     string
	ClientID, ClientSecret, RedirectURI, PostLogoutRedirectURI      string
	IdentityProviderHint                                            string
	ApplicationCode, EnvironmentCode, TenantID                      string
	Scopes                                                          []string
	SessionCookieName, PathPrefix                                   string
	SessionTTL, AuthorizationRefreshInterval, AuthorizationMaxStale time.Duration
	AuthorizationTimeout                                            time.Duration
	SessionSecure                                                   bool
	SessionEncryptionKey                                            []byte
	Store                                                           OIDCStore
	AuthorizationClient                                             AuthorizationContextClient
	// Audit 可选；非空时在登录/登出成功后向平台审计上报 auth.login / auth.logout。
	Audit AuditReporter
}

type oidcIDTokenClaims struct {
	Subject, IdentityID, TenantID, PersonID, Nonce, TokenUse string
	SessionID                                                string
}

// UnmarshalJSON 恢复持久化主体，同时保持旧快照兼容。
// 参数：data为待解析的序列化主体。
// 返回值：无；参数、校验、存储或依赖失败时返回错误。
func (claims *oidcIDTokenClaims) UnmarshalJSON(data []byte) error {
	type rawClaims struct {
		SessionID  string `json:"sid"`
		Subject    string `json:"sub"`
		IdentityID string `json:"identity_id"`
		TenantID   string `json:"tenant_id"`
		PersonID   string `json:"person_id"`
		Nonce      string `json:"nonce"`
		TokenUse   string `json:"token_use"`
	}
	var raw rawClaims
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*claims = oidcIDTokenClaims{Subject: raw.Subject, IdentityID: raw.IdentityID, TenantID: raw.TenantID, PersonID: raw.PersonID, Nonce: raw.Nonce, TokenUse: raw.TokenUse, SessionID: raw.SessionID}
	return nil
}

type oidcUserInfoClaims struct {
	Subject, Name, PreferredUsername, Email string
}

// UnmarshalJSON 恢复持久化主体，同时保持旧快照兼容。
// 参数：data为待解析的序列化主体。
// 返回值：无；参数、校验、存储或依赖失败时返回错误。
func (claims *oidcUserInfoClaims) UnmarshalJSON(data []byte) error {
	var raw struct {
		Subject           string `json:"sub"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
		Email             string `json:"email"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*claims = oidcUserInfoClaims{raw.Subject, raw.Name, raw.PreferredUsername, raw.Email}
	return nil
}

type OIDCAuthenticator struct {
	options            OIDCOptions
	provider           *oidc.Provider
	verifier           *oidc.IDTokenVerifier
	oauth2Config       oauth2.Config
	endSessionEndpoint string
	httpClient         *http.Client
	authorization      AuthorizationContextClient
	catalog            AuthorizationCatalog
	store              OIDCStore
	codec              *secretCodec
	audit              AuditReporter
	now                func() time.Time
}

// NewOIDCAuthenticator 装配原OIDC认证、持久存储及可选中央会话依赖。
// 参数：ctx为调用上下文与取消边界。
//
//	options为本应用认证及运行选项。
//
// 返回值：装配后的应用或认证实例；配置与依赖装配失败时返回错误。
func NewOIDCAuthenticator(ctx context.Context, options OIDCOptions) (*OIDCAuthenticator, error) {
	if options.CentralSessionEnabled && options.CentralClient == nil {
		public := options.CentralSessionPublicBaseURL
		if public == "" {
			redirect, err := url.Parse(options.RedirectURI)
			if err != nil {
				return nil, err
			}
			public = redirect.Scheme + "://" + redirect.Host
		}
		client, err := central.NewHTTPClient(options.PlatformBaseURL, public, options.ClientID, options.RedirectURI)
		if err != nil {
			return nil, err
		}
		options.CentralClient = client
	}
	if options.Store == nil {
		return nil, errors.New("OIDC persistent store is required")
	}
	if options.CentralClient != nil {
		if _, ok := options.Store.(centralOIDCStore); !ok {
			return nil, errors.New("central sessions require transactional storage")
		}
	}
	codec, err := newSecretCodec(options.SessionEncryptionKey)
	if err != nil {
		return nil, err
	}
	catalog, err := LoadAuthorizationCatalog()
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: options.AuthorizationTimeout}
	if options.BackchannelBaseURL != "" {
		publicURL, parseErr := url.Parse(strings.TrimRight(options.Issuer, "/"))
		if parseErr != nil {
			return nil, fmt.Errorf("parse OIDC issuer: %w", parseErr)
		}
		backchannelURL, parseErr := url.Parse(strings.TrimRight(options.BackchannelBaseURL, "/"))
		if parseErr != nil {
			return nil, fmt.Errorf("parse OIDC backchannel URL: %w", parseErr)
		}
		httpClient.Transport = &backchannelTransport{base: http.DefaultTransport, public: publicURL, backchannel: backchannelURL}
	}
	oidcContext := oidc.ClientContext(ctx, httpClient)
	provider, err := oidc.NewProvider(oidcContext, strings.TrimRight(options.Issuer, "/"))
	if err != nil {
		return nil, fmt.Errorf("load OIDC discovery: %w", err)
	}
	var discovery struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&discovery); err != nil {
		return nil, fmt.Errorf("decode OIDC discovery: %w", err)
	}
	authorization := options.AuthorizationClient
	if authorization == nil {
		authorization, err = NewHTTPAuthorizationContextClient(strings.TrimRight(options.PlatformBaseURL, "/")+"/oauth2/authorization-context", options.AuthorizationTimeout, nil)
		if err != nil {
			return nil, err
		}
	}
	return &OIDCAuthenticator{
		options: options, provider: provider, verifier: provider.Verifier(&oidc.Config{ClientID: options.ClientID}),
		oauth2Config:       oauth2.Config{ClientID: options.ClientID, ClientSecret: options.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: options.RedirectURI, Scopes: options.Scopes},
		endSessionEndpoint: strings.TrimSpace(discovery.EndSessionEndpoint), httpClient: httpClient,
		authorization: authorization, catalog: catalog, store: options.Store, codec: codec, audit: options.Audit, now: time.Now,
	}, nil
}

// Authenticate 校验当前浏览器会话、中央期限与业务授权。
// 参数：ctx为调用上下文与取消边界。
//
//	request为经过调用方校验的请求内容。
//
// 返回值：经过核验的主体或会话；明确失效与暂时依赖错误分别返回。
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, request *http.Request) (application.Principal, error) {
	cookie, err := request.Cookie(a.options.SessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return application.Principal{}, ErrUnauthenticated
	}
	now := a.now().UTC()
	record, err := a.store.FindSession(ctx, tokenHash(cookie.Value), now)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return application.Principal{}, ErrUnauthenticated
		}
		return application.Principal{}, ErrAuthorizationServiceUnavailable
	}
	if record.TenantID != a.options.TenantID {
		return application.Principal{}, ErrUnauthenticated
	}
	if a.options.CentralClient != nil {
		return a.authenticateCentral(ctx, record, now)
	}
	principal, err := principalFromJSON(record.PrincipalJSON)
	if err != nil || principal.IdentityID != record.IdentityID || principal.TenantID != record.TenantID {
		_ = a.store.RevokeSession(ctx, record.SessionIDHash, now)
		return application.Principal{}, ErrUnauthenticated
	}
	refreshDue := now.Sub(record.AuthorizationCheckedAt) >= a.options.AuthorizationRefreshInterval || !record.TokenExpiresAt.After(now)
	if !refreshDue {
		return principal, nil
	}
	updated, refreshed, err := a.refreshAuthorization(ctx, record, principal, now)
	if err == nil {
		if refreshed {
			if err := a.store.UpdateSession(ctx, updated); err != nil {
				return application.Principal{}, ErrAuthorizationServiceUnavailable
			}
			return principalFromJSON(updated.PrincipalJSON)
		}
		return principal, nil
	}
	if errors.Is(err, ErrAuthorizationForbidden) || errors.Is(err, ErrAuthorizationUnauthorized) || errors.Is(err, ErrAuthorizationInvalid) || refreshTokenWasRejected(err) {
		_ = a.store.RevokeSessionsForIdentity(ctx, record.TenantID, record.IdentityID, now)
		return application.Principal{}, ErrUnauthenticated
	}
	if errors.Is(err, ErrAuthorizationUnavailable) && safeStaleMethod(request.Method) && now.Sub(record.AuthorizationCheckedAt) <= a.options.AuthorizationMaxStale && record.TokenExpiresAt.After(now) {
		return principal, nil
	}
	return application.Principal{}, ErrAuthorizationServiceUnavailable
}

// Login 建立原state、nonce和PKCE事务并进入可信登录流程。
// 参数：writer为HTTP响应写入器。
//
//	request为经过调用方校验的请求内容。
//
// 返回值：无。
func (a *OIDCAuthenticator) Login(writer http.ResponseWriter, request *http.Request) {
	now := a.now().UTC()
	state, stateErr := randomValue(32)
	nonce, nonceErr := randomValue(32)
	if stateErr != nil || nonceErr != nil {
		http.Error(writer, "cannot start login", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	nonceCiphertext, err := a.codec.encrypt(nonce)
	if err != nil {
		http.Error(writer, "cannot start login", http.StatusInternalServerError)
		return
	}
	verifierCiphertext, err := a.codec.encrypt(verifier)
	if err != nil {
		http.Error(writer, "cannot start login", http.StatusInternalServerError)
		return
	}
	if err := a.store.SaveLoginTransaction(request.Context(), LoginTransactionRecord{
		ForceLogin: strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("prompt")), "login"),
		StateHash:  tokenHash(state), TenantID: a.options.TenantID, NonceCiphertext: nonceCiphertext,
		CodeVerifierCiphertext: verifierCiphertext, ReturnPath: a.publicPath("/"), ExpiresAt: now.Add(loginTransactionTTL), CreatedAt: now,
	}); err != nil {
		http.Error(writer, "cannot start login", http.StatusServiceUnavailable)
		return
	}
	target := a.authorizationURL(state, nonce, verifier, strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("prompt")), "login"))
	if a.options.CentralClient != nil {
		http.SetCookie(writer, a.stateCookie(state, 600))
		target = a.options.CentralClient.BeginURL(state, nonce)
	}
	http.Redirect(writer, request, target, http.StatusFound)
}

// authorizationURL 使用原事务构造提供方授权地址。
// 参数：state为原始登录事务随机状态。
//
//	nonce为原OIDC登录随机校验材料。
//	verifier为原PKCE秘密。
//	forceLogin为是否要求重新认证。
//
// 返回值：绑定原登录事务的授权地址；构造失败按原错误模型返回。
func (a *OIDCAuthenticator) authorizationURL(state, nonce, verifier string, forceLogin bool) string {
	options := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	if a.options.CentralClient != nil {
		options = append(options, oauth2.SetAuthURLParam("claims", `{"id_token":{"auth_time":{"essential":true}}}`))
	}
	if forceLogin {
		options = append(options, oauth2.SetAuthURLParam("prompt", "login"))
		if a.options.CentralClient != nil {
			options = append(options, oauth2.SetAuthURLParam("max_age", "0"))
		}
	}
	if hint := strings.TrimSpace(a.options.IdentityProviderHint); hint != "" {
		options = append(options, oauth2.SetAuthURLParam("kc_idp_hint", hint))
	}
	return a.oauth2Config.AuthCodeURL(state, options...)
}

// Callback 消费原登录事务，验证身份并创建受中央期限约束的本地会话。
// 参数：writer为HTTP响应写入器。
//
//	request为经过调用方校验的请求内容。
//
// 返回值：无。
func (a *OIDCAuthenticator) Callback(writer http.ResponseWriter, request *http.Request) {
	if a.options.CentralClient != nil {
		cookie, err := request.Cookie(a.options.SessionCookieName + "_oidc_state")
		if err != nil || cookie.Value == "" || cookie.Value != request.URL.Query().Get("state") {
			a.writeCallbackError(writer, request, "browser_state", http.StatusUnauthorized, ErrUnauthenticated)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		if ticket := request.URL.Query().Get("session_link_ticket"); ticket != "" {
			a.acceptSessionLink(writer, request, cookie.Value, ticket)
			return
		}
		http.SetCookie(writer, a.stateCookie("", -1))
	}
	if oauthError := strings.TrimSpace(request.URL.Query().Get("error")); oauthError != "" {
		a.writeCallbackError(writer, request, "authorization", http.StatusUnauthorized, errors.New(oauthError))
		return
	}
	state, code := strings.TrimSpace(request.URL.Query().Get("state")), strings.TrimSpace(request.URL.Query().Get("code"))
	if state == "" || code == "" {
		a.writeCallbackError(writer, request, "callback_parameters", http.StatusUnauthorized, errors.New("missing code or state"))
		return
	}
	now := a.now().UTC()
	transaction, err := a.store.ConsumeLoginTransaction(request.Context(), tokenHash(state), now)
	if err != nil || transaction.TenantID != a.options.TenantID {
		a.writeCallbackError(writer, request, "login_state", http.StatusUnauthorized, err)
		return
	}
	nonce, err := a.codec.decrypt(transaction.NonceCiphertext)
	if err != nil {
		a.writeCallbackError(writer, request, "login_state", http.StatusUnauthorized, err)
		return
	}
	verifier, err := a.codec.decrypt(transaction.CodeVerifierCiphertext)
	if err != nil {
		a.writeCallbackError(writer, request, "login_state", http.StatusUnauthorized, err)
		return
	}
	oidcContext := oidc.ClientContext(request.Context(), a.httpClient)
	token, err := a.oauth2Config.Exchange(oidcContext, code, oauth2.VerifierOption(verifier))
	if err != nil {
		a.writeCallbackError(writer, request, "token_exchange", http.StatusUnauthorized, err)
		return
	}
	identity, rawIDToken, idExpiry, err := a.verifyIDToken(oidcContext, token, nonce)
	if err != nil {
		a.writeCallbackError(writer, request, "id_token", http.StatusUnauthorized, err)
		return
	}
	var binding central.Binding
	boundSID := identity.SessionID
	if a.options.CentralClient != nil {
		ticket, decryptErr := a.codec.decrypt(transaction.CentralTicketCiphertext)
		if decryptErr != nil || ticket == "" {
			a.writeCallbackError(writer, request, "central_ticket", http.StatusUnauthorized, ErrUnauthenticated)
			return
		}
		binding, err = a.options.CentralClient.Complete(request.Context(), ticket, token.AccessToken, rawIDToken)
		if err == nil {
			err = central.Validate(binding.Context, a.centralExpectation(), identity.IdentityID)
		}
		if err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, central.ErrInactive) || errors.Is(err, central.ErrBindingConflict) {
				status = http.StatusUnauthorized
			}
			a.writeCallbackError(writer, request, "central_binding", status, errors.New("central session binding failed"))
			return
		}
	}
	contextSnapshot, token, identity, rawIDToken, idExpiry, err := a.resolveInitialAuthorization(oidcContext, token, identity, rawIDToken, idExpiry)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, ErrAuthorizationForbidden) {
			status = http.StatusForbidden
		} else if errors.Is(err, ErrAuthorizationInvalid) {
			status = http.StatusForbidden
		} else if errors.Is(err, ErrAuthorizationUnavailable) {
			status = http.StatusServiceUnavailable
		}
		a.writeCallbackError(writer, request, "authorization_context", status, err)
		return
	}
	if a.options.CentralClient != nil && (boundSID == "" || identity.SessionID != boundSID) {
		a.writeCallbackError(writer, request, "central_sid", http.StatusUnauthorized, ErrUnauthenticated)
		return
	}
	principal, err := principalFromAuthorizationContext(identity, contextSnapshot, a.catalog, a.options.ClientID, a.options.ApplicationCode, a.options.EnvironmentCode)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrAuthorizationInvalid) {
			status = http.StatusUnauthorized
		}
		a.writeCallbackError(writer, request, "local_authorization", status, err)
		return
	}
	principal.DisplayName, principal.UserName, principal.Email, err = a.loadCurrentUserInfo(oidcContext, token, principal.Subject)
	if err != nil {
		a.writeCallbackError(writer, request, "userinfo", http.StatusServiceUnavailable, err)
		return
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		a.writeCallbackError(writer, request, "token_exchange", http.StatusUnauthorized, errOIDCRefreshTokenIsMissing)
		return
	}
	rawSession, err := randomValue(48)
	if err != nil {
		a.writeCallbackError(writer, request, "session", http.StatusInternalServerError, err)
		return
	}
	record, err := a.newSessionRecord(rawSession, principal, token, rawIDToken, earliestExpiry(idExpiry, token.Expiry), now)
	if err != nil {
		a.writeCallbackError(writer, request, "session", http.StatusInternalServerError, err)
		return
	}
	if a.options.CentralClient != nil {
		record.CentralBindingCiphertext, err = a.codec.encrypt(binding.Token)
		if err != nil {
			a.writeCallbackError(writer, request, "central_storage", http.StatusServiceUnavailable, err)
			return
		}
		record.CentralSessionID = binding.Context.SessionID
		record.SessionExpiresAt = binding.Context.AbsoluteExpiresAt
		record.OIDCSessionID = identity.SessionID
	}
	if err := a.store.CreateSession(request.Context(), record); err != nil {
		a.writeCallbackError(writer, request, "session", http.StatusServiceUnavailable, err)
		return
	}
	a.reportAuth(request, AuditEvent{ActorID: principal.UserID, ActorName: principal.DisplayName, Action: "auth.login", ResourceType: "auth_session", Result: "SUCCESS", RiskLevel: "LOW"})
	http.SetCookie(writer, a.sessionCookie(rawSession, record.SessionExpiresAt))
	http.Redirect(writer, request, transaction.ReturnPath, http.StatusFound)
}

func (a *OIDCAuthenticator) verifyIDToken(ctx context.Context, token *oauth2.Token, expectedNonce string) (compactIdentity, string, time.Time, error) {
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		return compactIdentity{}, "", time.Time{}, errors.New("OIDC response has no ID token")
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return compactIdentity{}, "", time.Time{}, fmt.Errorf("verify ID token: %w", err)
	}
	var claims oidcIDTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return compactIdentity{}, "", time.Time{}, fmt.Errorf("decode ID token: %w", err)
	}
	identity, err := validateCompactIDTokenClaims(claims, expectedNonce, a.options.TenantID)
	if err != nil {
		return compactIdentity{}, "", time.Time{}, err
	}
	return identity, rawIDToken, idToken.Expiry.UTC(), nil
}

// validateCompactIDTokenClaims 核验身份令牌的用途、应用、主体及时间边界。
// 参数：claims为已经完成签名及用途校验的声明。
//
//	expectedNonce为原始登录事务保存的nonce。
//	expectedTenantID为服务端配置的预期租户。
//
// 返回值：与该调用匹配的核验或持久化结果；失败保留原有错误语义。
func validateCompactIDTokenClaims(claims oidcIDTokenClaims, expectedNonce, expectedTenantID string) (compactIdentity, error) {
	subject := strings.TrimSpace(claims.Subject)
	identityID := strings.TrimSpace(claims.IdentityID)
	if identityID == "" {
		return compactIdentity{}, errors.New("OIDC ID token identity_id is required")
	}
	if subject == "" || subject != claims.Subject || claims.TokenUse != "id_token" ||
		claims.TenantID != expectedTenantID || expectedNonce != "" && claims.Nonce != expectedNonce {
		return compactIdentity{}, errors.New("OIDC ID token stable identity claims are invalid")
	}
	return compactIdentity{Subject: subject, IdentityID: identityID, TenantID: claims.TenantID, PersonID: strings.TrimSpace(claims.PersonID), SessionID: claims.SessionID}, nil
}

func (a *OIDCAuthenticator) resolveInitialAuthorization(ctx context.Context, token *oauth2.Token, identity compactIdentity, rawIDToken string, expiry time.Time) (AuthorizationContext, *oauth2.Token, compactIdentity, string, time.Time, error) {
	snapshot, err := a.authorization.Resolve(ctx, token.AccessToken)
	if !errors.Is(err, ErrAuthorizationUnauthorized) {
		return snapshot, token, identity, rawIDToken, expiry, err
	}
	refreshed, refreshErr := a.refreshTokens(ctx, token)
	if refreshErr != nil {
		return AuthorizationContext{}, token, identity, rawIDToken, expiry, refreshErr
	}
	refreshedIdentity, refreshedIDToken, refreshedExpiry, verifyErr := a.verifyIDToken(ctx, refreshed, "")
	if verifyErr != nil || refreshedIdentity != identity {
		return AuthorizationContext{}, token, identity, rawIDToken, expiry, ErrAuthorizationUnauthorized
	}
	snapshot, err = a.authorization.Resolve(ctx, refreshed.AccessToken)
	return snapshot, refreshed, refreshedIdentity, refreshedIDToken, refreshedExpiry, err
}

// refreshAuthorization 复核权限并兼容旧模式刷新；中央模式由独立第一事务轮换凭据。
// 参数：ctx为请求，record/current为现有记录和主体，now为时钟，allowRefresh限制第二事务不再轮换。
// 返回值：新快照及变更标记；拒绝或依赖失败时不授予业务访问。
func (a *OIDCAuthenticator) refreshAuthorization(ctx context.Context, record SessionRecord, current application.Principal, now time.Time, allowRefresh ...bool) (SessionRecord, bool, error) {
	canRefresh := len(allowRefresh) == 0 || allowRefresh[0]
	accessToken, err := a.codec.decrypt(record.AccessTokenCiphertext)
	if err != nil {
		return record, false, ErrAuthorizationInvalid
	}
	refreshToken, err := a.codec.decrypt(record.RefreshTokenCiphertext)
	if err != nil {
		return record, false, ErrAuthorizationInvalid
	}
	idToken, err := a.codec.decrypt(record.IDTokenCiphertext)
	if err != nil {
		return record, false, ErrAuthorizationInvalid
	}
	token := &oauth2.Token{AccessToken: accessToken, RefreshToken: refreshToken, Expiry: record.TokenExpiresAt}
	subject := firstNonEmpty(current.Subject, current.UserID)
	identity := compactIdentity{Subject: subject, IdentityID: current.IdentityID, TenantID: current.TenantID, PersonID: current.PersonID}
	refreshedTokens := false
	if !record.TokenExpiresAt.After(now) || a.options.CentralClient != nil && !record.TokenExpiresAt.After(now.Add(time.Minute)) {
		if !canRefresh {
			return record, false, ErrAuthorizationServiceUnavailable
		}
		token, err = a.refreshTokens(oidc.ClientContext(ctx, a.httpClient), token)
		if err != nil {
			return record, false, err
		}
		identity, idToken, record.TokenExpiresAt, err = a.verifyIDToken(oidc.ClientContext(ctx, a.httpClient), token, "")
		if err != nil {
			return record, false, err
		}
		refreshedTokens = true
		record.TokenExpiresAt = earliestExpiry(record.TokenExpiresAt, token.Expiry)
	}
	snapshot, err := a.authorization.Resolve(ctx, token.AccessToken)
	if errors.Is(err, ErrAuthorizationUnauthorized) && !refreshedTokens && canRefresh {
		token, err = a.refreshTokens(oidc.ClientContext(ctx, a.httpClient), token)
		if err != nil {
			return record, false, err
		}
		identity, idToken, record.TokenExpiresAt, err = a.verifyIDToken(oidc.ClientContext(ctx, a.httpClient), token, "")
		if err != nil {
			return record, false, err
		}
		refreshedTokens = true
		record.TokenExpiresAt = earliestExpiry(record.TokenExpiresAt, token.Expiry)
		snapshot, err = a.authorization.Resolve(ctx, token.AccessToken)
	}
	if err != nil {
		return record, false, err
	}
	principal, err := principalFromAuthorizationContext(identity, snapshot, a.catalog, a.options.ClientID, a.options.ApplicationCode, a.options.EnvironmentCode)
	if a.options.CentralClient != nil && refreshedTokens && (record.OIDCSessionID == "" || identity.SessionID != record.OIDCSessionID) {
		return record, false, ErrAuthorizationInvalid
	}
	if err != nil || principal.Subject != subject || principal.IdentityID != current.IdentityID || principal.TenantID != current.TenantID {
		if err == nil {
			err = ErrAuthorizationInvalid
		}
		return record, false, err
	}
	principal.DisplayName, principal.UserName, principal.Email = current.DisplayName, current.UserName, current.Email
	principalJSON, err := json.Marshal(principal)
	if err != nil {
		return record, false, ErrAuthorizationInvalid
	}
	accessCiphertext, err := a.codec.encrypt(token.AccessToken)
	if err != nil {
		return record, false, err
	}
	refreshCiphertext, err := a.codec.encrypt(firstNonEmpty(token.RefreshToken, refreshToken))
	if err != nil {
		return record, false, err
	}
	idCiphertext, err := a.codec.encrypt(idToken)
	if err != nil {
		return record, false, err
	}
	record.PrincipalJSON, record.PersonID = principalJSON, principal.PersonID
	record.AccessTokenCiphertext, record.RefreshTokenCiphertext, record.IDTokenCiphertext = accessCiphertext, refreshCiphertext, idCiphertext
	record.AuthorizationRevision, record.AuthorizationCheckedAt, record.UpdatedAt = principal.AuthorizationRevision, now, now
	return record, true, nil
}

func (a *OIDCAuthenticator) refreshTokens(ctx context.Context, token *oauth2.Token) (*oauth2.Token, error) {
	if token == nil || strings.TrimSpace(token.RefreshToken) == "" {
		return nil, errOIDCRefreshTokenIsMissing
	}
	seed := &oauth2.Token{RefreshToken: token.RefreshToken, Expiry: time.Now().Add(-time.Second)}
	refreshed, err := a.oauth2Config.TokenSource(ctx, seed).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh OIDC token: %w", err)
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = token.RefreshToken
	}
	return refreshed, nil
}

func (a *OIDCAuthenticator) newSessionRecord(rawSession string, principal application.Principal, token *oauth2.Token, rawIDToken string, tokenExpiry, now time.Time) (SessionRecord, error) {
	principalJSON, err := json.Marshal(principal)
	if err != nil {
		return SessionRecord{}, err
	}
	accessCiphertext, err := a.codec.encrypt(token.AccessToken)
	if err != nil {
		return SessionRecord{}, err
	}
	refreshCiphertext, err := a.codec.encrypt(token.RefreshToken)
	if err != nil {
		return SessionRecord{}, err
	}
	idCiphertext, err := a.codec.encrypt(rawIDToken)
	if err != nil {
		return SessionRecord{}, err
	}
	return SessionRecord{SessionIDHash: tokenHash(rawSession), TenantID: principal.TenantID, IdentityID: principal.IdentityID,
		PersonID: principal.PersonID, PrincipalJSON: principalJSON, AccessTokenCiphertext: accessCiphertext,
		RefreshTokenCiphertext: refreshCiphertext, IDTokenCiphertext: idCiphertext,
		AuthorizationRevision: principal.AuthorizationRevision, AuthorizationCheckedAt: now,
		TokenExpiresAt: tokenExpiry, SessionExpiresAt: now.Add(a.options.SessionTTL), CreatedAt: now, UpdatedAt: now}, nil
}

func principalFromJSON(raw []byte) (application.Principal, error) {
	var principal application.Principal
	if err := json.Unmarshal(raw, &principal); err != nil {
		return application.Principal{}, err
	}
	if principal.Subject == "" {
		// 主体与身份拆分前创建的会话，可在读取时升级，
		// 因为其旧版 UserID 就是平台身份 ID。
		principal.Subject = principal.UserID
	}
	if principal.IdentityID == "" || principal.UserID != principal.IdentityID || principal.TenantID == "" || principal.AuthorizationRevision == 0 {
		return application.Principal{}, ErrAuthorizationInvalid
	}
	return principal, nil
}

func (a *OIDCAuthenticator) loadCurrentUserInfo(ctx context.Context, token *oauth2.Token, expectedSubject string) (string, string, string, error) {
	info, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return "", "", "", fmt.Errorf("load OIDC UserInfo: %w", err)
	}
	var claims oidcUserInfoClaims
	if err := info.Claims(&claims); err != nil {
		return "", "", "", fmt.Errorf("decode OIDC UserInfo: %w", err)
	}
	if info.Subject != expectedSubject || claims.Subject != "" && claims.Subject != expectedSubject {
		return "", "", "", errors.New("OIDC UserInfo subject changed")
	}
	displayName := firstNonEmpty(strings.TrimSpace(claims.Name), strings.TrimSpace(claims.PreferredUsername), strings.TrimSpace(claims.Email), expectedSubject)
	return displayName, strings.TrimSpace(claims.PreferredUsername), strings.TrimSpace(claims.Email), nil
}

func (a *OIDCAuthenticator) Logout(writer http.ResponseWriter, request *http.Request) {
	idToken := a.clearLocalSession(writer, request)
	endpoint, err := url.Parse(a.endSessionEndpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		http.Redirect(writer, request, a.publicPath("/logged-out"), http.StatusFound)
		return
	}
	query := endpoint.Query()
	query.Set("client_id", a.options.ClientID)
	if idToken != "" {
		query.Set("id_token_hint", idToken)
	}
	if a.options.PostLogoutRedirectURI != "" {
		query.Set("post_logout_redirect_uri", a.options.PostLogoutRedirectURI)
	}
	endpoint.RawQuery = query.Encode()
	http.Redirect(writer, request, endpoint.String(), http.StatusFound)
}

func (a *OIDCAuthenticator) LogoutLocal(writer http.ResponseWriter, request *http.Request) {
	a.clearLocalSession(writer, request)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

// reportAuth 向平台审计上报登录/登出事件；审计未配置或上报失败均不影响认证流程。
func (a *OIDCAuthenticator) reportAuth(request *http.Request, event AuditEvent) {
	if a.audit == nil {
		return
	}
	_ = a.audit.Report(request.Context(), event)
}

func (a *OIDCAuthenticator) clearLocalSession(writer http.ResponseWriter, request *http.Request) string {
	var idToken string
	if cookie, err := request.Cookie(a.options.SessionCookieName); err == nil && cookie.Value != "" {
		now := a.now().UTC()
		identityID := ""
		if record, findErr := a.store.FindSession(request.Context(), tokenHash(cookie.Value), now); findErr == nil {
			idToken, _ = a.codec.decrypt(record.IDTokenCiphertext)
			identityID = record.IdentityID
		}
		_ = a.store.RevokeSession(request.Context(), tokenHash(cookie.Value), now)
		if identityID != "" {
			a.reportAuth(request, AuditEvent{ActorID: identityID, Action: "auth.logout", ResourceType: "auth_session", Result: "SUCCESS", RiskLevel: "LOW"})
		}
	}
	expired := a.sessionCookie("", time.Unix(1, 0))
	expired.MaxAge = -1
	http.SetCookie(writer, expired)
	return idToken
}

func (a *OIDCAuthenticator) sessionCookie(value string, expires time.Time) *http.Cookie {
	path := a.options.PathPrefix
	if path == "" {
		path = "/"
	}
	return &http.Cookie{Name: a.options.SessionCookieName, Value: value, Path: path, Expires: expires, HttpOnly: true, Secure: a.options.SessionSecure, SameSite: http.SameSiteLaxMode}
}

func (a *OIDCAuthenticator) PublicPath(path string) string { return a.publicPath(path) }

func (a *OIDCAuthenticator) publicPath(path string) string {
	prefix := strings.TrimRight(a.options.PathPrefix, "/")
	if path == "/" {
		return prefix + "/"
	}
	return prefix + "/" + strings.TrimLeft(path, "/")
}

func (a *OIDCAuthenticator) writeCallbackError(writer http.ResponseWriter, request *http.Request, stage string, status int, cause error) {
	slog.ErrorContext(request.Context(), "contract OIDC callback failed", "stage", stage, "issuer", a.options.Issuer, "client_id", a.options.ClientID, "environment", a.options.EnvironmentCode, "error", cause)
	// 失败时不能再返回 纯文本 "Unauthorized/Forbidden"：用户卡在地址栏里看不到任何可操作信息。
	// 跳到平台顶层 /access-error 错误页，由前端 SubsystemAccessErrorView 渲染：
	// - 必须用相对路径让浏览器继续走同一个会话，避免跳出当前 origin；
	// - 必须不带子系统路径前缀（/contract_management/...），否则会触发路由守卫再次要求合同会话，
	//   形成死循环；/access-error 是平台共享路由，不需要子系统会话。
	// - 把 stage / status / request_id / from 全部塞进 query，便于前端展示 + 后端按 stage 定位。
	// - 302 Found 让浏览器 GET 重定向（POST 切到 GET 也安全）。
	query := url.Values{}
	query.Set("reason", "callback_failed")
	query.Set("stage", stage)
	query.Set("code", fmt.Sprintf("%d", status))
	if raw := request.URL.Path; raw != "" {
		query.Set("from", raw)
	}
	if requestID := strings.TrimSpace(request.Header.Get("X-Request-ID")); requestID != "" {
		query.Set("request_id", requestID)
	}
	http.Redirect(writer, request, "/access-error?"+query.Encode(), http.StatusFound)
}

func refreshTokenWasRejected(err error) bool {
	if errors.Is(err, errOIDCRefreshTokenIsMissing) {
		return true
	}
	var retrieveError *oauth2.RetrieveError
	return errors.As(err, &retrieveError) && retrieveError.ErrorCode == "invalid_grant"
}

func safeStaleMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func earliestExpiry(values ...time.Time) time.Time {
	var result time.Time
	for _, value := range values {
		if !value.IsZero() && (result.IsZero() || value.Before(result)) {
			result = value.UTC()
		}
	}
	return result
}

type backchannelTransport struct {
	base                http.RoundTripper
	public, backchannel *url.URL
}

func (transport *backchannelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != transport.public.Scheme || request.URL.Host != transport.public.Host {
		return transport.base.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = transport.backchannel.Scheme, transport.backchannel.Host
	return transport.base.RoundTrip(clone)
}

func randomValue(size int) (string, error) {
	if size < 16 {
		return "", errors.New("random value size is too small")
	}
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
