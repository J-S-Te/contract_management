// Package auditoutbox 投递事务内写下的业务审计事件（批次 2）。
//
// 业务事务只负责把事件写入 con_audit_outbox；真正投递由本包的后台循环完成。
// 平台不可用时事件留在本地，按退避重试；进程崩溃后租约到期可被重新领取。
package auditoutbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
	"github.com/j-s-te/contract-management/internal/lifecycle"
)

// Entry 是一条待投递的审计事件。
type Entry struct {
	ID, TenantID, EventID, ActorID, Action, ResourceType, ResourceID, RequestID string
	LoginIP, ClientIP, IPSource, Result                                         string
	Attempts                                                                    uint
	// CreatedAt 是事务内事件的发生时间；重试必须复用它，不能每次重新取 now。
	CreatedAt time.Time
}

// Store 负责领取与回写；实现必须用租约保证同一事件只被一个 worker 投递。
type Store interface {
	Claim(ctx context.Context, workerID string, limit int, now time.Time) ([]Entry, error)
	MarkDelivered(ctx context.Context, id string, now time.Time) error
	MarkFailed(ctx context.Context, id string, code string, retryAt time.Time, now time.Time) error
}

// Dispatcher 依配置的周期投递，达到上限后放弃并保留错误码供告警。
type Dispatcher struct {
	Store       Store
	Reporter    platform.AuditReporter
	WorkerID    string
	Poll        time.Duration
	BatchSize   int
	MaxAttempts uint
	MaxBackoff  time.Duration
	Logger      *slog.Logger
}

// Run 持续投递，直到 ctx 取消。
func (d *Dispatcher) Run(ctx context.Context) {
	poll := d.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		d.deliverOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverOnce 领取一批事件并逐条投递；单条失败不影响其余事件。
// ctx 已取消时不再领取新批次；已领取的批次由 deliverEntry 用独立 ctx 完成落库。
func (d *Dispatcher) deliverOnce(ctx context.Context) {
	if d.Store == nil || d.Reporter == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	entries, err := d.Store.Claim(ctx, d.WorkerID, d.batchSize(), now)
	if err != nil {
		d.logger().ErrorContext(ctx, "claim audit outbox events", "error", err)
		return
	}
	// 批次排空 ctx：一旦收到取消，本批剩余项共用同一个 5s 上界，
	// 不允许逐条重新计时把退出拖过进程预算。
	var (
		drainCtx    context.Context
		cancelDrain context.CancelFunc
	)
	defer func() {
		if cancelDrain != nil {
			cancelDrain()
		}
	}()
	for _, entry := range entries {
		itemCtx := ctx
		if ctx.Err() != nil {
			if drainCtx == nil {
				drainCtx, cancelDrain = lifecycle.DrainContext(ctx)
			}
			itemCtx = drainCtx
		}
		d.deliverEntry(itemCtx, entry, now)
	}
}

// deliverEntry 投递一条已认领事件并回写终态。
// 参数：ctx 为进程 ctx（或在途批次的排空 ctx），entry 为已认领行，now 为批次时间。
// 返回值：无；投递失败保留在 outbox 按退避重试。
//
// 退出排空契约：ctx 取消后仍必须完成本条落库。回写用独立 ctx，否则
// MarkDelivered/MarkFailed 在已取消的 ctx 上二次失败，行停在 CLAIMED
// 直到租约过期，导致审计事件被延迟而不是被完成。
func (d *Dispatcher) deliverEntry(ctx context.Context, entry Entry, now time.Time) {
	occurredAt := entry.CreatedAt
	if occurredAt.IsZero() {
		occurredAt = now
	}
	event := platform.AuditEvent{
		EventID: entry.EventID, OccurredAt: occurredAt,
		ActorID: entry.ActorID, Action: entry.Action,
		ResourceType: entry.ResourceType, ResourceID: entry.ResourceID,
		RequestID: entry.RequestID, CorrelationID: entry.RequestID,
		Result: entry.Result, LoginIP: entry.LoginIP, ClientIP: entry.ClientIP, IPSource: entry.IPSource,
	}
	reportErr := d.Reporter.Report(ctx, event)
	persistCtx, cancelPersist := lifecycle.PersistContext(ctx)
	defer cancelPersist()
	if reportErr != nil {
		// 失败的尝试一律保留在 outbox：业务已经提交，审计不能丢。
		code := "PLATFORM_AUDIT_UNAVAILABLE"
		retryAt := now.Add(d.backoff(entry.Attempts + 1))
		if err := d.Store.MarkFailed(persistCtx, entry.ID, code, retryAt, now); err != nil {
			d.logger().ErrorContext(persistCtx, "mark audit outbox failure", "error", err, "event_id", entry.EventID)
		}
		return
	}
	if err := d.Store.MarkDelivered(persistCtx, entry.ID, now); err != nil {
		d.logger().ErrorContext(persistCtx, "mark audit outbox delivered", "error", err, "event_id", entry.EventID)
	}
}

func (d *Dispatcher) batchSize() int {
	if d.BatchSize <= 0 || d.BatchSize > 200 {
		return 50
	}
	return d.BatchSize
}

// backoff 指数退避，上限可配置；投递失败永远不会立即热循环。
func (d *Dispatcher) backoff(attempts uint) time.Duration {
	base := time.Second
	for i := uint(1); i < attempts && i < 8; i++ {
		base *= 2
	}
	if max := d.MaxBackoff; max > 0 && base > max {
		return max
	}
	return base
}

func (d *Dispatcher) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}
