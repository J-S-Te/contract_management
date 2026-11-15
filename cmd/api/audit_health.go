package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/j-s-te/contract-management/internal/auditoutbox"
	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

func reportAuditOutboxHealth(ctx context.Context, store *auditoutbox.GORMStore, reporter platform.AuditReporter, logger *slog.Logger) {
	if store == nil || reporter == nil {
		return
	}
	health, ok := reporter.(platform.OutboxHealthReporter)
	if !ok {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		pending, dead, err := store.DeliveryStatusCounts(ctx)
		if err == nil {
			reportCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if reportErr := health.ReportOutboxHealth(reportCtx, pending, dead, time.Time{}); reportErr != nil {
				logger.Warn("report audit outbox health failed", "error", reportErr)
			}
			cancel()
		} else {
			logger.Warn("read audit outbox health failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
