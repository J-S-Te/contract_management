package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/j-s-te/contract-management/internal/application"
)

type Config struct {
	HTTPAddress       string
	MySQLDSN          string
	PlatformBaseURL   string
	TemporalAddress   string
	TemporalNamespace string
	TemporalTaskQueue string
	TemporalAPIKey    string
	TemporalTLS       bool
	NodeTimeout       time.Duration
	ReminderInterval  time.Duration
	ArchiveCron       string
	Approvers         application.StaticApprovers
}

func Load() (Config, error) {
	c := Config{
		HTTPAddress: env("HTTP_ADDRESS", ":8081"), PlatformBaseURL: env("PLATFORM_BASE_URL", "http://localhost:8080"),
		TemporalAddress: env("TEMPORAL_ADDRESS", "localhost:7233"), TemporalNamespace: env("TEMPORAL_NAMESPACE", "default"), TemporalTaskQueue: env("TEMPORAL_TASK_QUEUE", "contract-management"),
		NodeTimeout: duration("APPROVAL_NODE_TIMEOUT", 72*time.Hour), ReminderInterval: duration("APPROVAL_REMINDER_INTERVAL", 24*time.Hour), TemporalAPIKey: os.Getenv("TEMPORAL_API_KEY"),
		ArchiveCron: env("ARCHIVE_CRON_SCHEDULE", "0 16 * * *"),
	}
	c.MySQLDSN = os.Getenv("MYSQL_DSN")
	if c.MySQLDSN == "" {
		return c, fmt.Errorf("MYSQL_DSN is required")
	}
	c.TemporalTLS, _ = strconv.ParseBool(env("TEMPORAL_TLS", "false"))
	if raw := os.Getenv("APPROVER_ROLE_ASSIGNMENTS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &c.Approvers); err != nil {
			return c, fmt.Errorf("APPROVER_ROLE_ASSIGNMENTS_JSON: %w", err)
		}
	} else {
		c.Approvers = application.StaticApprovers{}
	}
	return c, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func duration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
