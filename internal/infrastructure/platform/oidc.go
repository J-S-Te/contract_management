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
	"golang.org/x/oauth2"
)

const loginTransactionTTL = 10 * time.Minute

var (
	ErrUnauthenticated                 = errors.New("unauthenticated")
	ErrAuthorizationServiceUnavailable = errors.New("authorization service unavailable")
	errOIDCRefreshTokenIsMissing       = errors.New("OIDC refresh token is missing")
)

type OIDCOptions struct {
	Issuer, BackchannelBaseURL, PlatformBaseURL                     string
	ClientID, ClientSecret, RedirectURI, PostLogoutRedirectURI      string
	ApplicationCode, EnvironmentCode, TenantID                      string
	Scopes                                                          []string
	SessionCookieName, PathPrefix                                   string
	SessionTTL, AuthorizationRefreshInterval, AuthorizationMaxStale time.Duration
	AuthorizationTimeout                                            time.Duration
	SessionSecure                                                   bool
	SessionEncryptionKey                                            []byte
	Store                                                           OIDCStore
	AuthorizationClient                                             AuthorizationContextClient
}

type oidcIDTokenClaims struct {
	Subject, IdentityID, TenantID, PersonID, Nonce, TokenUse string
}

func (claims *oidcIDTokenClaims) UnmarshalJSON(data []byte) error {
	type rawClaims struct {
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
	*claims = oidcIDTokenClaims{Subject: raw.Subject, IdentityID: raw.IdentityID, TenantID: raw.TenantID, PersonID: raw.PersonID, Nonce: raw.Nonce, TokenUse: raw.TokenUse}
	return nil
}

type oidcUserInfoClaims struct {
	Subject, Name, PreferredUsername, Email string
}

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
	now                func() time.Time
}

func NewOIDCAuthenticator(ctx context.Context, options OIDCOptions) (*OIDCAuthenticator, error) {
	if options.Store == nil {
		return nil, errors.New("OIDC persistent store is required")
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
		authorization: authorization, catalog: catalog, store: options.Store, codec: codec, now: time.Now,
	}, nil
}

func (a *OIDCAuthenticator) Authenticate(ctx context.Context, request *http.Request) (application.Principal, error) {
	cookie, err := request.Cookie(a.options.SessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return application.Principal{}, ErrUnauthenticated
	}
	now := a.now().UTC()
	record, err := a.store.FindSession(ctx, tokenHash(cookie.Value), now)
	if err != nil || record.TenantID != a.options.TenantID {
		return application.Principal{}, ErrUnauthenticated
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
		StateHash: tokenHash(state), TenantID: a.options.TenantID, NonceCiphertext: nonceCiphertext,
		CodeVerifierCiphertext: verifierCiphertext, ReturnPath: a.publicPath("/"), ExpiresAt: now.Add(loginTransactionTTL), CreatedAt: now,
	}); err != nil {
		http.Error(writer, "cannot start login", http.StatusServiceUnavailable)
		return
	}
	target := a.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(writer, request, target, http.StatusFound)
}

func (a *OIDCAuthenticator) Callback(writer http.ResponseWriter, request *http.Request) {
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
	principal, err := principalFromAuthorizationContext(identity, contextSnapshot, a.catalog, a.options.ClientID, a.options.ApplicationCode, a.options.EnvironmentCode)
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrAuthorizationInvalid) {
			status = http.StatusUnauthorized
		}
		a.writeCallbackError(writer, request, "local_authorization", status, err)
		return
	}
	principal.DisplayName, principal.UserName, principal.Email, err = a.loadCurrentUserInfo(oidcContext, token, principal.UserID)
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
	if err := a.store.CreateSession(request.Context(), record); err != nil {
		a.writeCallbackError(writer, request, "session", http.StatusServiceUnavailable, err)
		return
	}
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

func validateCompactIDTokenClaims(claims oidcIDTokenClaims, expectedNonce, expectedTenantID string) (compactIdentity, error) {
	subject := strings.TrimSpace(claims.Subject)
	identityID := strings.TrimSpace(claims.IdentityID)
	if identityID == "" {
		identityID = subject
	}
	if subject == "" || subject != claims.Subject || identityID != subject || claims.TokenUse != "id_token" ||
		claims.TenantID != expectedTenantID || expectedNonce != "" && claims.Nonce != expectedNonce {
		return compactIdentity{}, errors.New("OIDC ID token stable identity claims are invalid")
	}
	return compactIdentity{Subject: subject, IdentityID: identityID, TenantID: claims.TenantID, PersonID: strings.TrimSpace(claims.PersonID)}, nil
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

func (a *OIDCAuthenticator) refreshAuthorization(ctx context.Context, record SessionRecord, current application.Principal, now time.Time) (SessionRecord, bool, error) {
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
	identity := compactIdentity{Subject: current.UserID, IdentityID: current.IdentityID, TenantID: current.TenantID, PersonID: current.PersonID}
	refreshedTokens := false
	if !record.TokenExpiresAt.After(now) {
		token, err = a.refreshTokens(oidc.ClientContext(ctx, a.httpClient), token)
		if err != nil {
			return record, false, err
		}
		identity, idToken, record.TokenExpiresAt, err = a.verifyIDToken(oidc.ClientContext(ctx, a.httpClient), token, "")
		if err != nil {
			return record, false, err
		}
		refreshedTokens = true
	}
	snapshot, err := a.authorization.Resolve(ctx, token.AccessToken)
	if errors.Is(err, ErrAuthorizationUnauthorized) && !refreshedTokens {
		token, err = a.refreshTokens(oidc.ClientContext(ctx, a.httpClient), token)
		if err != nil {
			return record, false, err
		}
		identity, idToken, record.TokenExpiresAt, err = a.verifyIDToken(oidc.ClientContext(ctx, a.httpClient), token, "")
		if err != nil {
			return record, false, err
		}
		refreshedTokens = true
		snapshot, err = a.authorization.Resolve(ctx, token.AccessToken)
	}
	if err != nil {
		return record, false, err
	}
	principal, err := principalFromAuthorizationContext(identity, snapshot, a.catalog, a.options.ClientID, a.options.ApplicationCode, a.options.EnvironmentCode)
	if err != nil || principal.IdentityID != current.IdentityID || principal.TenantID != current.TenantID {
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

func (a *OIDCAuthenticator) clearLocalSession(writer http.ResponseWriter, request *http.Request) string {
	var idToken string
	if cookie, err := request.Cookie(a.options.SessionCookieName); err == nil && cookie.Value != "" {
		now := a.now().UTC()
		if record, findErr := a.store.FindSession(request.Context(), tokenHash(cookie.Value), now); findErr == nil {
			idToken, _ = a.codec.decrypt(record.IDTokenCiphertext)
		}
		_ = a.store.RevokeSession(request.Context(), tokenHash(cookie.Value), now)
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
	http.Error(writer, http.StatusText(status), status)
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
