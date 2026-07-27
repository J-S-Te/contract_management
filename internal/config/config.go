package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

type Config struct {
	HTTPAddress               string
	MySQLDSN                  string
	PlatformBaseURL           string
	PlatformSessionCookieName string
	PlatformAuditClientID     string
	PlatformAuditClientSecret string
	PlatformApplicationCode   string
	PlatformEnvironmentCode   string
	TemporalAddress           string
	TemporalNamespace         string
	TemporalTaskQueue         string
	TemporalAPIKey            string
	TemporalTLS               bool
	NodeTimeout               time.Duration
	ReminderInterval          time.Duration
	ArchiveCron               string
	Approvers                 application.StaticApprovers
}

func Load() (Config, error) {
	c := Config{
		HTTPAddress: env("HTTP_ADDRESS", ":8081"), PlatformBaseURL: env("PLATFORM_BASE_URL", "http://localhost:8080"), PlatformSessionCookieName: env("AUTH_SESSION_COOKIE_NAME", "bp_session"),
		PlatformAuditClientID: os.Getenv("PLATFORM_AUDIT_CLIENT_ID"), PlatformAuditClientSecret: os.Getenv("PLATFORM_AUDIT_CLIENT_SECRET"), PlatformApplicationCode: os.Getenv("PLATFORM_APPLICATION_CODE"), PlatformEnvironmentCode: os.Getenv("PLATFORM_ENVIRONMENT_CODE"),
		TemporalAddress: env("TEMPORAL_ADDRESS", "localhost:7233"), TemporalNamespace: env("TEMPORAL_NAMESPACE", "default"), TemporalTaskQueue: env("TEMPORAL_TASK_QUEUE", "contract-management"),
		TemporalAPIKey: os.Getenv("TEMPORAL_API_KEY"),
		ArchiveCron:    env("ARCHIVE_CRON_SCHEDULE", "0 16 * * *"),
	}
	var err error
	if c.NodeTimeout, err = duration("APPROVAL_NODE_TIMEOUT", 72*time.Hour); err != nil {
		return c, err
	}
	if c.ReminderInterval, err = duration("APPROVAL_REMINDER_INTERVAL", 24*time.Hour); err != nil {
		return c, err
	}
	c.MySQLDSN = os.Getenv("MYSQL_DSN")
	if c.MySQLDSN == "" {
		return c, fmt.Errorf("MYSQL_DSN is required")
	}
	if c.TemporalTLS, err = strconv.ParseBool(env("TEMPORAL_TLS", "false")); err != nil {
		return c, fmt.Errorf("TEMPORAL_TLS: %w", err)
	}
	if raw := os.Getenv("APPROVER_ROLE_ASSIGNMENTS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Approvers); err != nil {
			return c, fmt.Errorf("APPROVER_ROLE_ASSIGNMENTS_JSON: %w", err)
		}
	} else {
		c.Approvers = application.StaticApprovers{}
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) validate() error {
	if strings.TrimSpace(c.HTTPAddress) == "" {
		return fmt.Errorf("HTTP_ADDRESS must not be empty")
	}
	platformURL, err := url.ParseRequestURI(c.PlatformBaseURL)
	if err != nil || (platformURL.Scheme != "http" && platformURL.Scheme != "https") ||
		platformURL.Host == "" || platformURL.User != nil || platformURL.RawQuery != "" ||
		platformURL.Fragment != "" || (platformURL.Path != "" && platformURL.Path != "/") {
		return fmt.Errorf("PLATFORM_BASE_URL must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	if strings.TrimSpace(c.PlatformSessionCookieName) == "" {
		return fmt.Errorf("AUTH_SESSION_COOKIE_NAME must not be empty")
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
	return nil
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
