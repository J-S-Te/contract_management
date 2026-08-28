package bootstrap

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/j-s-te/contract-management/internal/config"
	"go.temporal.io/sdk/client"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func OpenDatabase(ctx context.Context, dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{PrepareStmt: true, TranslateError: true})
	if err != nil {
		return nil, fmt.Errorf("open mysql with GORM: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get mysql connection pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(30)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connect mysql: %w", err)
	}
	return db, nil
}

func CloseDatabase(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func OpenTemporal(ctx context.Context, cfg config.Config, metricHandlers ...client.MetricsHandler) (client.Client, error) {
	options := client.Options{HostPort: cfg.TemporalAddress, Namespace: cfg.TemporalNamespace}
	if len(metricHandlers) > 1 {
		return nil, fmt.Errorf("open Temporal accepts at most one metrics handler")
	}
	if len(metricHandlers) == 1 {
		if metricHandlers[0] == nil {
			return nil, fmt.Errorf("Temporal metrics handler must not be nil")
		}
		options.MetricsHandler = metricHandlers[0]
	}
	if cfg.TemporalTLS {
		options.ConnectionOptions.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		options.ConnectionOptions.TLSDisabled = true
	}
	if cfg.TemporalAPIKey != "" {
		options.Credentials = client.NewAPIKeyStaticCredentials(cfg.TemporalAPIKey)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return client.DialContext(dialCtx, options)
}
