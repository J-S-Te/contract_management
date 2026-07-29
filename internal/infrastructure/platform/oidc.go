package platform

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/j-s-te/contract-management/internal/application"
	"golang.org/x/oauth2"
)

const loginTransactionTTL = 10 * time.Minute

var ErrUnauthenticated = errors.New("unauthenticated")

type OIDCOptions struct {
	Issuer                string
	BackchannelBaseURL    string
	ClientID              string
	ClientSecret          string
	RedirectURI           string
	PostLogoutRedirectURI string
	Scopes                []string
	TenantID              string
	SessionCookieName     string
	SessionTTL            time.Duration
	SessionSecure         bool
	PathPrefix            string
}

type loginTransaction struct {
	Nonce        string
	CodeVerifier string
	ExpiresAt    time.Time
}

type localSession struct {
	Principal application.Principal
	IDToken   string
	ExpiresAt time.Time
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

// platformAllPermissions is the contract-side expansion of the platform's protected SYS-004
// admin marker. It grants actions only; contract data remains owner-scoped in the application
// service for every role.
var platformAllPermissions = []string{
	"dashboard",
	"contract.read", "contract.create", "contract.edit", "contract.delete",
	"customer.read", "customer.create", "customer.edit", "customer.delete",
	"contract_type.manage", "contract_template.read", "contract_template.manage",
	"approval.view", "approval.process", "approval.manage", "approval_rule.manage",
	"user.manage", "audit.view", "audit.read",
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
	mutex        sync.Mutex
	transactions map[string]loginTransaction
	sessions     map[string]localSession
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
		transactions: make(map[string]loginTransaction), sessions: make(map[string]localSession),
	}
	return authenticator, nil
}

func (a *OIDCAuthenticator) Authenticate(_ context.Context, request *http.Request) (application.Principal, error) {
	cookie, err := request.Cookie(a.options.SessionCookieName)
	if err != nil || cookie.Value == "" {
		return application.Principal{}, ErrUnauthenticated
	}
	now := a.now().UTC()
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.cleanupLocked(now)
	session, exists := a.sessions[cookie.Value]
	if !exists || !session.ExpiresAt.After(now) {
		return application.Principal{}, ErrUnauthenticated
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

	sessionID, err := randomValue(32)
	if err != nil {
		http.Error(writer, "cannot create local session", http.StatusInternalServerError)
		return
	}
	session := localSession{
		Principal: principal,
		IDToken:   rawIDToken, ExpiresAt: now.Add(a.options.SessionTTL),
	}
	a.mutex.Lock()
	a.cleanupLocked(now)
	a.sessions[sessionID] = session
	a.mutex.Unlock()
	http.SetCookie(writer, a.sessionCookie(sessionID, session.ExpiresAt))
	http.Redirect(writer, request, a.publicPath("/"), http.StatusFound)
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
	permissions := make(map[string]bool, len(claims.Permissions)+len(platformAllPermissions))
	for _, permission := range claims.Permissions {
		if permission == "" || permission != strings.TrimSpace(permission) {
			return application.Principal{}, errors.New("OIDC permissions are malformed")
		}
		permissions[permission] = true
	}
	if permissions["all"] {
		for _, permission := range platformAllPermissions {
			permissions[permission] = true
		}
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
	var idToken string
	if cookie, err := request.Cookie(a.options.SessionCookieName); err == nil {
		a.mutex.Lock()
		if session, exists := a.sessions[cookie.Value]; exists {
			idToken = session.IDToken
			delete(a.sessions, cookie.Value)
		}
		a.mutex.Unlock()
	}
	expired := a.sessionCookie("", time.Unix(1, 0))
	expired.MaxAge = -1
	http.SetCookie(writer, expired)

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
