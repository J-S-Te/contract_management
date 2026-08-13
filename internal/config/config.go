package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddress               string
	MySQLDSN                  string
	PlatformBaseURL           string
	OIDCIssuer                string
	OIDCBackchannelBaseURL    string
	OIDCClientID              string
	OIDCClientSecret          string
	OIDCRedirectURI           string
	OIDCPostLogoutRedirectURI string
	OIDCScopes                []string
	OIDCTenantID              string
	OIDCSessionCookieName     string
	OIDCSessionTTL            time.Duration
	OIDCAuthorizationRefresh  time.Duration
	OIDCAuthorizationTimeout  time.Duration
	OIDCAuthorizationMaxStale time.Duration
	OIDCSessionEncryptionKey  []byte
	OIDCSessionSecure         bool
	AppPublicURL              string
	AppPathPrefix             string
	PlatformAuditClientID     string
	PlatformAuditClientSecret string
	PlatformApplicationCode   string
	PlatformEnvironmentCode   string
	PlatformApplicationID     string
	PlatformCatalogSync       bool
	PlatformCatalogClientID   string
	PlatformCatalogSecret     string
	PlatformPersonnelClientID string
	PlatformPersonnelSecret   string
	TemporalAddress           string
	TemporalNamespace         string
	TemporalTaskQueue         string
	TemporalAPIKey            string
	TemporalTLS               bool
	NodeTimeout               time.Duration
	ReminderInterval          time.Duration
	ArchiveCron               string
	ProjectIntegrationEnabled bool
	ProjectAPIBaseURL         string
	ProjectIntegrationPoll    time.Duration
	ProjectIntegrationRetries uint
}

func Load() (Config, error) {
	c := Config{
		HTTPAddress: env("HTTP_ADDRESS", ":8081"), PlatformBaseURL: env("PLATFORM_BASE_URL", "http://localhost:8080"),
		OIDCIssuer: os.Getenv("OIDC_ISSUER"), OIDCClientID: os.Getenv("OIDC_CLIENT_ID"),
		OIDCBackchannelBaseURL: os.Getenv("OIDC_BACKCHANNEL_BASE_URL"),
		OIDCClientSecret:       os.Getenv("OIDC_CLIENT_SECRET"), OIDCRedirectURI: os.Getenv("OIDC_REDIRECT_URI"),
		OIDCPostLogoutRedirectURI: os.Getenv("OIDC_POST_LOGOUT_REDIRECT_URI"),
		OIDCScopes:                fields(env("OIDC_SCOPES", "openid profile")), OIDCTenantID: os.Getenv("OIDC_TENANT_ID"),
		OIDCSessionCookieName: env("OIDC_SESSION_COOKIE_NAME", "contract_management_session"),
		AppPublicURL:          os.Getenv("APP_PUBLIC_URL"), AppPathPrefix: env("APP_PATH_PREFIX", "/contract_management"),
		PlatformAuditClientID: os.Getenv("PLATFORM_AUDIT_CLIENT_ID"), PlatformAuditClientSecret: os.Getenv("PLATFORM_AUDIT_CLIENT_SECRET"), PlatformApplicationCode: os.Getenv("PLATFORM_APPLICATION_CODE"), PlatformEnvironmentCode: os.Getenv("PLATFORM_ENVIRONMENT_CODE"),
		PlatformApplicationID:     os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_APPLICATION_ID"),
		PlatformCatalogClientID:   os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_ID"),
		PlatformCatalogSecret:     os.Getenv("PLATFORM_AUTHORIZATION_CATALOG_CLIENT_SECRET"),
		PlatformPersonnelClientID: os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_ID"),
		PlatformPersonnelSecret:   os.Getenv("PLATFORM_PERSONNEL_DIRECTORY_CLIENT_SECRET"),
		TemporalAddress:           env("TEMPORAL_ADDRESS", "localhost:7233"), TemporalNamespace: env("TEMPORAL_NAMESPACE", "default"), TemporalTaskQueue: env("TEMPORAL_TASK_QUEUE", "contract-management"),
		TemporalAPIKey: os.Getenv("TEMPORAL_API_KEY"), ArchiveCron: env("ARCHIVE_CRON_SCHEDULE", "0 16 * * *"),
		ProjectAPIBaseURL: env("PROJECT_API_BASE_URL", "http://localhost:8082"),
	}
	var err error
	if c.NodeTimeout, err = duration("APPROVAL_NODE_TIMEOUT", 72*time.Hour); err != nil {
		return c, err
	}
	if c.ReminderInterval, err = duration("APPROVAL_REMINDER_INTERVAL", 24*time.Hour); err != nil {
		return c, err
	}
	if c.OIDCSessionTTL, err = duration("OIDC_SESSION_TTL", 8*time.Hour); err != nil {
		return c, err
	}
	if c.OIDCAuthorizationRefresh, err = duration("OIDC_AUTHORIZATION_REFRESH_INTERVAL", time.Minute); err != nil {
		return c, err
	}
	if c.OIDCAuthorizationTimeout, err = duration("OIDC_AUTHORIZATION_CONTEXT_TIMEOUT", 10*time.Second); err != nil {
		return c, err
	}
	if c.OIDCAuthorizationMaxStale, err = duration("OIDC_AUTHORIZATION_MAX_STALE", 2*time.Minute); err != nil {
		return c, err
	}
	encodedKey := strings.TrimSpace(os.Getenv("OIDC_SESSION_ENCRYPTION_KEY_BASE64"))
	if encodedKey == "" {
		return c, fmt.Errorf("OIDC_SESSION_ENCRYPTION_KEY_BASE64 is required")
	}
	c.OIDCSessionEncryptionKey, err = base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(c.OIDCSessionEncryptionKey) != 32 {
		return c, fmt.Errorf("OIDC_SESSION_ENCRYPTION_KEY_BASE64 must decode to exactly 32 bytes")
	}
	if c.OIDCSessionSecure, err = strconv.ParseBool(env("OIDC_SESSION_COOKIE_SECURE", "true")); err != nil {
		return c, fmt.Errorf("OIDC_SESSION_COOKIE_SECURE: %w", err)
	}
	c.MySQLDSN = os.Getenv("MYSQL_DSN")
	if c.MySQLDSN == "" {
		return c, fmt.Errorf("MYSQL_DSN is required")
	}
	if c.TemporalTLS, err = strconv.ParseBool(env("TEMPORAL_TLS", "false")); err != nil {
		return c, fmt.Errorf("TEMPORAL_TLS: %w", err)
	}
	if c.PlatformCatalogSync, err = strconv.ParseBool(env("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("PLATFORM_AUTHORIZATION_CATALOG_SYNC_ENABLED: %w", err)
	}
	if c.ProjectIntegrationEnabled, err = strconv.ParseBool(env("PROJECT_INTEGRATION_ENABLED", "false")); err != nil {
		return c, fmt.Errorf("PROJECT_INTEGRATION_ENABLED: %w", err)
	}
	if c.ProjectIntegrationPoll, err = duration("PROJECT_INTEGRATION_POLL_INTERVAL", 2*time.Second); err != nil {
		return c, err
	}
	retries, parseErr := strconv.ParseUint(env("PROJECT_INTEGRATION_MAX_ATTEMPTS", "20"), 10, 32)
	if parseErr != nil || retries == 0 {
		return c, fmt.Errorf("PROJECT_INTEGRATION_MAX_ATTEMPTS must be a positive integer")
	}
	c.ProjectIntegrationRetries = uint(retries)
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) validate() error {
	if strings.TrimSpace(c.HTTPAddress) == "" {
		return fmt.Errorf("HTTP_ADDRESS must not be empty")
	}
	if c.ProjectIntegrationEnabled {
		if !validHTTPOrigin(c.ProjectAPIBaseURL) {
			return fmt.Errorf("PROJECT_API_BASE_URL must be an HTTP(S) origin")
		}
	}
	if c.ProjectIntegrationPoll <= 0 {
		return fmt.Errorf("PROJECT_INTEGRATION_POLL_INTERVAL must be positive")
	}
	platformURL, err := url.ParseRequestURI(c.PlatformBaseURL)
	if err != nil || (platformURL.Scheme != "http" && platformURL.Scheme != "https") ||
		platformURL.Host == "" || platformURL.User != nil || platformURL.RawQuery != "" ||
		platformURL.Fragment != "" || (platformURL.Path != "" && platformURL.Path != "/") {
		return fmt.Errorf("PLATFORM_BASE_URL must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	issuerURL, err := url.ParseRequestURI(c.OIDCIssuer)
	if err != nil || (issuerURL.Scheme != "http" && issuerURL.Scheme != "https") || issuerURL.Host == "" ||
		issuerURL.User != nil || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
		return fmt.Errorf("OIDC_ISSUER must be a valid HTTP(S) URL")
	}
	if c.OIDCBackchannelBaseURL != "" {
		backchannelURL, parseErr := url.ParseRequestURI(c.OIDCBackchannelBaseURL)
		if parseErr != nil || (backchannelURL.Scheme != "http" && backchannelURL.Scheme != "https") ||
			backchannelURL.Host == "" || backchannelURL.User != nil || backchannelURL.RawQuery != "" ||
			backchannelURL.Fragment != "" || (backchannelURL.Path != "" && backchannelURL.Path != "/") {
			return fmt.Errorf("OIDC_BACKCHANNEL_BASE_URL must be an HTTP(S) origin")
		}
	}
	if strings.TrimSpace(c.OIDCClientID) == "" || strings.TrimSpace(c.OIDCClientSecret) == "" ||
		strings.TrimSpace(c.OIDCRedirectURI) == "" || strings.TrimSpace(c.OIDCTenantID) == "" {
		return fmt.Errorf("OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, OIDC_REDIRECT_URI and OIDC_TENANT_ID are required")
	}
	if len(c.OIDCScopes) == 0 || !contains(c.OIDCScopes, "openid") {
		return fmt.Errorf("OIDC_SCOPES must include openid")
	}
	if strings.TrimSpace(c.OIDCSessionCookieName) == "" || c.OIDCSessionTTL <= 0 ||
		c.OIDCAuthorizationRefresh <= 0 || c.OIDCAuthorizationRefresh >= c.OIDCSessionTTL ||
		c.OIDCAuthorizationTimeout <= 0 || c.OIDCAuthorizationMaxStale < 0 || c.OIDCAuthorizationMaxStale >= c.OIDCSessionTTL {
		return fmt.Errorf("OIDC session cookie name, positive TTL and a shorter positive authorization refresh interval are required")
	}
	if strings.TrimSpace(c.PlatformApplicationCode) == "" || strings.TrimSpace(c.PlatformEnvironmentCode) == "" {
		return fmt.Errorf("PLATFORM_APPLICATION_CODE and PLATFORM_ENVIRONMENT_CODE are required for OIDC authorization binding")
	}
	if c.AppPathPrefix == "/" || !strings.HasPrefix(c.AppPathPrefix, "/") ||
		(c.AppPathPrefix != "" && strings.HasSuffix(c.AppPathPrefix, "/")) {
		return fmt.Errorf("APP_PATH_PREFIX must be empty or a non-root absolute path without trailing slash")
	}
	if strings.TrimSpace(c.AppPublicURL) == "" {
		return fmt.Errorf("APP_PUBLIC_URL is required")
	}
	if strings.TrimSpace(c.TemporalAddress) == "" || strings.TrimSpace(c.TemporalNamespace) == "" || strings.TrimSpace(c.TemporalTaskQueue) == "" {
		return fmt.Errorf("Temporal address, namespace and task queue must not be empty")
	}
	if c.NodeTimeout <= 0 || c.ReminderInterval <= 0 || c.ReminderInterval >= c.NodeTimeout {
		return fmt.Errorf("approval durations must be positive and reminder interval must be shorter than node timeout")
	}
	hasAuditClientID := strings.TrimSpace(c.PlatformAuditClientID) != ""
	hasAuditClientSecret := strings.TrimSpace(c.PlatformAuditClientSecret) != ""
	if hasAuditClientID != hasAuditClientSecret {
		return fmt.Errorf("platform audit configuration must provide client ID and client secret together")
	}
	if hasAuditClientID && (strings.TrimSpace(c.PlatformApplicationCode) == "" || strings.TrimSpace(c.PlatformEnvironmentCode) == "") {
		return fmt.Errorf("platform audit configuration must provide client ID, client secret, application code and environment code together")
	}
	if c.PlatformCatalogSync && (strings.TrimSpace(c.PlatformApplicationID) == "" ||
		strings.TrimSpace(c.PlatformCatalogClientID) == "" || strings.TrimSpace(c.PlatformCatalogSecret) == "") {
		return fmt.Errorf("platform authorization catalog synchronization requires application ID, client ID and client secret")
	}
	if (strings.TrimSpace(c.PlatformPersonnelClientID) == "") != (strings.TrimSpace(c.PlatformPersonnelSecret) == "") {
		return fmt.Errorf("platform personnel directory configuration must provide client ID and secret together")
	}
	return nil
}

func fields(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == ',' })
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validHTTPOrigin(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/")
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func duration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}
