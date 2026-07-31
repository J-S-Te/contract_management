package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/j-s-te/contract-management/internal/application"
	"golang.org/x/oauth2"
)

const (
	loginTransactionTTL               = 10 * time.Minute
	authorizationRefreshRetryInterval = 5 * time.Second
)

var (
	ErrUnauthenticated           = errors.New("unauthenticated")
	errOIDCRefreshTokenIsMissing = errors.New("OIDC refresh token is missing")
)

type OIDCOptions struct {
	Issuer                       string
	BackchannelBaseURL           string
	ClientID                     string
	ClientSecret                 string
	RedirectURI                  string
	PostLogoutRedirectURI        string
	Scopes                       []string
	TenantID                     string
	SessionCookieName            string
	SessionTTL                   time.Duration
	AuthorizationRefreshInterval time.Duration
	SessionSecure                bool
	PathPrefix                   string
}

type loginTransaction struct {
	Nonce        string
	CodeVerifier string
	ExpiresAt    time.Time
}

type localSession struct {
	mutex          sync.Mutex
	Principal      application.Principal
	IDToken        string
	Token          *oauth2.Token
	RefreshedAt    time.Time
	RefreshRetryAt time.Time
	TokenExpiresAt time.Time
	ExpiresAt      time.Time
}

type platformIDTokenClaims struct {
	Subject        string   `json:"sub"`
	Nonce          string   `json:"nonce"`
	TenantID       string   `json:"tenant_id"`
	Roles          []string `json:"roles"`
	Permissions    []string `json:"permissions"`
	RoleConfigHash string   `json:"role_config_hash"`
	AuthzRevision  uint64   `json:"authz_revision"`
}

type platformUserInfoClaims struct {
	Subject            string                      `json:"sub"`
	Name               string                      `json:"name"`
	PersonnelDirectory []application.UserReference `json:"personnel_directory"`
}

// OIDCAuthenticator owns the contract system's OIDC login transactions and independent local
// sessions. Browser requests never reuse the platform bp_session cookie.
type OIDCAuthenticator struct {
	options      OIDCOptions
	provider     *oidc.Provider
	verifier     *oidc.IDTokenVerifier
	oauth2Config oauth2.Config
	httpClient   *http.Client
	now          func() time.Time
	refresh      func(context.Context, *localSession, time.Time) error
	mutex        sync.Mutex
	transactions map[string]loginTransaction
	sessions     map[string]*localSession
}

func NewOIDCAuthenticator(ctx context.Context, options OIDCOptions) (*OIDCAuthenticator, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if options.BackchannelBaseURL != "" {
		publicURL, err := url.Parse(strings.TrimRight(options.Issuer, "/"))
		if err != nil {
			return nil, fmt.Errorf("parse OIDC issuer: %w", err)
		}
		backchannelURL, err := url.Parse(strings.TrimRight(options.BackchannelBaseURL, "/"))
		if err != nil {
			return nil, fmt.Errorf("parse OIDC backchannel URL: %w", err)
		}
		httpClient.Transport = &backchannelTransport{
			base: http.DefaultTransport, public: publicURL, backchannel: backchannelURL,
		}
	}
	oidcContext := oidc.ClientContext(ctx, httpClient)
	provider, err := oidc.NewProvider(oidcContext, strings.TrimRight(options.Issuer, "/"))
	if err != nil {
		return nil, fmt.Errorf("load OIDC discovery: %w", err)
	}
	authenticator := &OIDCAuthenticator{
		options: options, provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: options.ClientID}),
		oauth2Config: oauth2.Config{
			ClientID: options.ClientID, ClientSecret: options.ClientSecret,
			Endpoint: provider.Endpoint(), RedirectURL: options.RedirectURI, Scopes: options.Scopes,
		},
		httpClient: httpClient, now: time.Now,
		transactions: make(map[string]loginTransaction), sessions: make(map[string]*localSession),
	}
	return authenticator, nil
}

func (a *OIDCAuthenticator) Authenticate(ctx context.Context, request *http.Request) (application.Principal, error) {
	cookie, err := request.Cookie(a.options.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return application.Principal{}, ErrUnauthenticated
	}
	now := a.now().UTC()
	a.mutex.Lock()
	a.cleanupLocked(now)
	session, exists := a.sessions[cookie.Value]
	a.mutex.Unlock()
	if !exists {
		return application.Principal{}, ErrUnauthenticated
	}
	session.mutex.Lock()
	defer session.mutex.Unlock()
	if !session.ExpiresAt.After(now) {
		a.deleteSession(cookie.Value, session)
		return application.Principal{}, ErrUnauthenticated
	}
	tokenExpired := !session.TokenExpiresAt.After(now)
	refreshDue := now.Sub(session.RefreshedAt) >= a.options.AuthorizationRefreshInterval || tokenExpired
	refreshAllowed := !session.RefreshRetryAt.After(now) || tokenExpired
	if refreshDue && refreshAllowed {
		refresh := a.refresh
		if refresh == nil {
			refresh = a.refreshSession
		}
		if err := refresh(ctx, session, now); err != nil {
			if tokenExpired || refreshTokenWasRejected(err) {
				a.deleteSession(cookie.Value, session)
				return application.Principal{}, ErrUnauthenticated
			}
			session.RefreshRetryAt = now.Add(authorizationRefreshRetryInterval)
			return session.Principal, nil
		}
		session.RefreshRetryAt = time.Time{}
	}
	return session.Principal, nil
}

func (a *OIDCAuthenticator) Login(writer http.ResponseWriter, request *http.Request) {
	now := a.now().UTC()
	state, err := randomValue(32)
	if err != nil {
		http.Error(writer, "cannot start login", http.StatusInternalServerError)
		return
	}
	nonce, err := randomValue(32)
	if err != nil {
		http.Error(writer, "cannot start login", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	a.mutex.Lock()
	a.cleanupLocked(now)
	a.transactions[state] = loginTransaction{Nonce: nonce, CodeVerifier: verifier, ExpiresAt: now.Add(loginTransactionTTL)}
	a.mutex.Unlock()

	target := a.oauth2Config.AuthCodeURL(
		state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(writer, request, target, http.StatusFound)
}

func (a *OIDCAuthenticator) Callback(writer http.ResponseWriter, request *http.Request) {
	if oauthError := strings.TrimSpace(request.URL.Query().Get("error")); oauthError != "" {
		http.Error(writer, "OIDC authorization failed: "+oauthError, http.StatusUnauthorized)
		return
	}
	state := strings.TrimSpace(request.URL.Query().Get("state"))
	code := strings.TrimSpace(request.URL.Query().Get("code"))
	if state == "" || code == "" {
		http.Error(writer, "missing OIDC code or state", http.StatusBadRequest)
		return
	}

	now := a.now().UTC()
	a.mutex.Lock()
	a.cleanupLocked(now)
	transaction, exists := a.transactions[state]
	delete(a.transactions, state)
	a.mutex.Unlock()
	if !exists || !transaction.ExpiresAt.After(now) {
		http.Error(writer, "invalid or expired OIDC state", http.StatusUnauthorized)
		return
	}

	oidcContext := oidc.ClientContext(request.Context(), a.httpClient)
	token, err := a.oauth2Config.Exchange(
		oidcContext, code,
		oauth2.VerifierOption(transaction.CodeVerifier),
	)
	if err != nil {
		http.Error(writer, "OIDC token exchange failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Error(writer, "OIDC response did not contain an ID token", http.StatusUnauthorized)
		return
	}
	idToken, err := a.verifier.Verify(oidcContext, rawIDToken)
	if err != nil {
		http.Error(writer, "OIDC ID token verification failed", http.StatusUnauthorized)
		return
	}
	var claims platformIDTokenClaims
	if err := idToken.Claims(&claims); err != nil || claims.Nonce != transaction.Nonce {
		http.Error(writer, "OIDC ID token claims are invalid", http.StatusUnauthorized)
		return
	}
	principal, err := principalFromPlatformClaims(claims, a.options.TenantID)
	if err != nil {
		http.Error(writer, "OIDC authorization claims are invalid", http.StatusUnauthorized)
		return
	}
	principal.DisplayName, principal.UserDirectory, err = a.loadUserInfo(oidcContext, token, principal.UserID)
	if err != nil {
		http.Error(writer, "OIDC user information is invalid", http.StatusUnauthorized)
		return
	}

	sessionID, err := randomValue(32)
	if err != nil {
		http.Error(writer, "cannot create local session", http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(token.RefreshToken) == "" {
		http.Error(writer, "OIDC response did not contain a refresh token", http.StatusUnauthorized)
		return
	}
	session := &localSession{
		Principal: principal, IDToken: rawIDToken, Token: token, RefreshedAt: now,
		TokenExpiresAt: idToken.Expiry, ExpiresAt: now.Add(a.options.SessionTTL),
	}
	a.mutex.Lock()
	a.cleanupLocked(now)
	a.sessions[sessionID] = session
	a.mutex.Unlock()
	http.SetCookie(writer, a.sessionCookie(sessionID, session.ExpiresAt))
	http.Redirect(writer, request, a.publicPath("/"), http.StatusFound)
}

func (a *OIDCAuthenticator) refreshSession(ctx context.Context, session *localSession, now time.Time) error {
	if session.Token == nil || strings.TrimSpace(session.Token.RefreshToken) == "" {
		return errOIDCRefreshTokenIsMissing
	}
	refreshSeed := &oauth2.Token{RefreshToken: session.Token.RefreshToken, Expiry: now.Add(-time.Second)}
	oidcContext := oidc.ClientContext(ctx, a.httpClient)
	token, err := a.oauth2Config.TokenSource(oidcContext, refreshSeed).Token()
	if err != nil {
		return fmt.Errorf("refresh OIDC token: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return errors.New("refreshed OIDC response did not contain an ID token")
	}
	idToken, err := a.verifier.Verify(oidcContext, rawIDToken)
	if err != nil {
		return fmt.Errorf("verify refreshed OIDC ID token: %w", err)
	}
	var claims platformIDTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return fmt.Errorf("decode refreshed OIDC ID token: %w", err)
	}
	principal, err := principalFromPlatformClaims(claims, a.options.TenantID)
	if err != nil {
		return err
	}
	principal.DisplayName, principal.UserDirectory, err = a.loadUserInfo(oidcContext, token, principal.UserID)
	if err != nil {
		return err
	}
	if principal.UserID != session.Principal.UserID || principal.TenantID != session.Principal.TenantID {
		return errors.New("refreshed OIDC subject changed")
	}
	if !idToken.Expiry.After(now) {
		return errors.New("refreshed OIDC ID token is expired")
	}
	session.Principal, session.IDToken, session.Token, session.RefreshedAt = principal, rawIDToken, token, now
	session.TokenExpiresAt = idToken.Expiry
	return nil
}

func refreshTokenWasRejected(err error) bool {
	if errors.Is(err, errOIDCRefreshTokenIsMissing) {
		return true
	}
	var retrieveError *oauth2.RetrieveError
	return errors.As(err, &retrieveError) && retrieveError.ErrorCode == "invalid_grant"
}

func (a *OIDCAuthenticator) loadUserInfo(ctx context.Context, token *oauth2.Token, expectedSubject string) (string, []application.UserReference, error) {
	if a.provider == nil || token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return "", nil, errors.New("OIDC UserInfo dependencies are incomplete")
	}
	info, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return "", nil, fmt.Errorf("load OIDC UserInfo: %w", err)
	}
	var claims platformUserInfoClaims
	if err := info.Claims(&claims); err != nil {
		return "", nil, fmt.Errorf("decode OIDC UserInfo: %w", err)
	}
	displayName := strings.TrimSpace(claims.Name)
	if info.Subject != expectedSubject || (claims.Subject != "" && claims.Subject != expectedSubject) || displayName == "" {
		return "", nil, errors.New("OIDC UserInfo subject or name is invalid")
	}
	return displayName, normalizePersonnelDirectory(claims.PersonnelDirectory), nil
}

func normalizePersonnelDirectory(entries []application.UserReference) []application.UserReference {
	directory := make([]application.UserReference, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		entry.UserID = strings.TrimSpace(entry.UserID)
		entry.DisplayName = strings.TrimSpace(entry.DisplayName)
		if entry.UserID == "" || entry.DisplayName == "" || seen[entry.UserID] {
			continue
		}
		entry.Roles = normalizeStrings(entry.Roles)
		seen[entry.UserID] = true
		directory = append(directory, entry)
	}
	return directory
}

func normalizeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (a *OIDCAuthenticator) deleteSession(id string, expected *localSession) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	if a.sessions[id] == expected {
		delete(a.sessions, id)
	}
}

func principalFromPlatformClaims(claims platformIDTokenClaims, expectedTenantID string) (application.Principal, error) {
	if strings.TrimSpace(claims.Subject) == "" || claims.Subject != strings.TrimSpace(claims.Subject) {
		return application.Principal{}, errors.New("OIDC subject is missing or malformed")
	}
	if claims.TenantID == "" || claims.TenantID != expectedTenantID {
		return application.Principal{}, errors.New("OIDC tenant does not match the configured tenant")
	}
	if len(claims.Roles) == 0 || claims.RoleConfigHash == "" || claims.AuthzRevision == 0 {
		return application.Principal{}, errors.New("OIDC authorization metadata is incomplete")
	}
	roles := make([]string, 0, len(claims.Roles))
	seenRoles := make(map[string]bool, len(claims.Roles))
	for _, role := range claims.Roles {
		if role == "" || role != strings.TrimSpace(role) || seenRoles[role] {
			return application.Principal{}, errors.New("OIDC roles are malformed")
		}
		seenRoles[role] = true
		roles = append(roles, role)
	}
	if len(claims.Permissions) == 0 {
		return application.Principal{}, errors.New("OIDC permissions are missing")
	}
	permissions := make(map[string]bool, len(claims.Permissions))
	for _, permission := range claims.Permissions {
		if permission == "" || permission != strings.TrimSpace(permission) {
			return application.Principal{}, errors.New("OIDC permissions are malformed")
		}
		if permission == "all" {
			return application.Principal{}, errors.New("OIDC wildcard permissions are unsupported")
		}
		permissions[permission] = true
	}
	return application.Principal{
		TenantID: claims.TenantID, UserID: claims.Subject, Roles: roles, Permissions: permissions,
		RoleConfigHash: claims.RoleConfigHash, AuthzRevision: claims.AuthzRevision,
	}, nil
}

type backchannelTransport struct {
	base        http.RoundTripper
	public      *url.URL
	backchannel *url.URL
}

func (transport *backchannelTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != transport.public.Scheme || request.URL.Host != transport.public.Host {
		return transport.base.RoundTrip(request)
	}
	clone := request.Clone(request.Context())
	clone.URL.Scheme = transport.backchannel.Scheme
	clone.URL.Host = transport.backchannel.Host
	return transport.base.RoundTrip(clone)
}

func (a *OIDCAuthenticator) Logout(writer http.ResponseWriter, request *http.Request) {
	idToken := a.clearLocalSession(writer, request)

	endpoint, err := url.Parse(strings.TrimRight(a.options.Issuer, "/") + "/oauth2/logout")
	if err != nil {
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
		if state, randomErr := randomValue(24); randomErr == nil {
			query.Set("state", state)
		}
	}
	endpoint.RawQuery = query.Encode()
	http.Redirect(writer, request, endpoint.String(), http.StatusFound)
}

// LogoutLocal clears only the contract subsystem session. It is used when the browser has
// switched platform accounts and must not revoke the newly authenticated platform session.
func (a *OIDCAuthenticator) LogoutLocal(writer http.ResponseWriter, request *http.Request) {
	a.clearLocalSession(writer, request)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusNoContent)
}

func (a *OIDCAuthenticator) clearLocalSession(writer http.ResponseWriter, request *http.Request) string {
	var idToken string
	if cookie, err := request.Cookie(a.options.SessionCookieName); err == nil {
		a.mutex.Lock()
		session := a.sessions[cookie.Value]
		delete(a.sessions, cookie.Value)
		a.mutex.Unlock()
		if session != nil {
			session.mutex.Lock()
			idToken = session.IDToken
			session.mutex.Unlock()
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
	return &http.Cookie{
		Name: a.options.SessionCookieName, Value: value, Path: path, Expires: expires,
		HttpOnly: true, Secure: a.options.SessionSecure, SameSite: http.SameSiteLaxMode,
	}
}

// PublicPath returns a browser-facing path under the configured portal prefix.
func (a *OIDCAuthenticator) PublicPath(path string) string {
	return a.publicPath(path)
}

func (a *OIDCAuthenticator) publicPath(path string) string {
	prefix := strings.TrimRight(a.options.PathPrefix, "/")
	if path == "/" {
		return prefix + "/"
	}
	return prefix + "/" + strings.TrimLeft(path, "/")
}

func (a *OIDCAuthenticator) cleanupLocked(now time.Time) {
	for state, transaction := range a.transactions {
		if !transaction.ExpiresAt.After(now) {
			delete(a.transactions, state)
		}
	}
	for id, session := range a.sessions {
		if !session.ExpiresAt.After(now) {
			delete(a.sessions, id)
		}
	}
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
