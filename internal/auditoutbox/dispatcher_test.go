package auditoutbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/j-s-te/contract-management/internal/infrastructure/platform"
)

type fakeStore struct {
	entries   []Entry
	delivered []string
	failed    []string
	failedAt  []time.Time
}

func (f *fakeStore) Claim(context.Context, string, int, time.Time) ([]Entry, error) {
	entries := f.entries
	f.entries = nil
	return entries, nil
}
func (f *fakeStore) MarkDelivered(_ context.Context, id string, _ time.Time) error {
	f.delivered = append(f.delivered, id)
	return nil
}
func (f *fakeStore) MarkFailed(_ context.Context, id, _ string, retryAt, _ time.Time) error {
	f.failed = append(f.failed, id)
	f.failedAt = append(f.failedAt, retryAt)
	return nil
}

type fakeReporter struct {
	err    error
	events []platform.AuditEvent
}

func (f *fakeReporter) Report(_ context.Context, event platform.AuditEvent) error {
	f.events = append(f.events, event)
	return f.err
}

// 平台不可用时事件必须留在 outbox 重试，并带上退避时间；业务早已提交，审计不能丢。
func TestDeliverRetriesWhenPlatformUnavailable(t *testing.T) {
	store := &fakeStore{entries: []Entry{{ID: "row-1", EventID: "event-1", Action: "CONTRACT_MANAGEMENT:STATUS_CHANGE", ClientIP: "10.8.0.5", IPSource: "TRUSTED_PROXY", Result: "SUCCESS"}}}
	reporter := &fakeReporter{err: errors.New("platform down")}
	dispatcher := &Dispatcher{Store: store, Reporter: reporter, WorkerID: "w1", BatchSize: 10, MaxAttempts: 5}
	now := time.Now().UTC()

	dispatcher.deliverOnce(context.Background())

	if len(store.failed) != 1 || store.failed[0] != "row-1" {
		t.Fatalf("failed = %v，投递失败必须回写 outbox", store.failed)
	}
	if len(store.delivered) != 0 {
		t.Fatal("失败事件不得标记为已投递")
	}
	if !store.failedAt[0].After(now) {
		t.Fatalf("retryAt = %s，必须晚于当前时间形成退避", store.failedAt[0])
	}
	if len(reporter.events) != 1 || reporter.events[0].ClientIP != "10.8.0.5" || reporter.events[0].IPSource != "TRUSTED_PROXY" {
		t.Fatalf("上报事件缺少来源信息: %#v", reporter.events)
	}
}

func TestDeliverMarksSuccess(t *testing.T) {
	occurredAt := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	store := &fakeStore{entries: []Entry{{ID: "row-2", EventID: "event-2", Action: "A", Result: "SUCCESS", CreatedAt: occurredAt}}}
	reporter := &fakeReporter{}
	dispatcher := &Dispatcher{Store: store, Reporter: reporter, WorkerID: "w1"}

	dispatcher.deliverOnce(context.Background())

	if len(store.delivered) != 1 || store.delivered[0] != "row-2" {
		t.Fatalf("delivered = %v", store.delivered)
	}
	if len(store.failed) != 0 {
		t.Fatalf("failed = %v", store.failed)
	}
	if len(reporter.events) != 1 || reporter.events[0].EventID != "event-2" || !reporter.events[0].OccurredAt.Equal(occurredAt) {
		t.Fatalf("投递必须复用持久化 event_id/occurred_at: %#v", reporter.events)
	}
}

// 指数退避必须单调递增且有上限，避免平台长时间不可用时热循环。
func TestBackoffGrowsAndCaps(t *testing.T) {
	dispatcher := &Dispatcher{MaxBackoff: 16 * time.Second}
	previous := time.Duration(0)
	for attempt := uint(1); attempt <= 8; attempt++ {
		value := dispatcher.backoff(attempt)
		if value <= 0 {
			t.Fatalf("backoff(%d) = %s", attempt, value)
		}
		if value > dispatcher.MaxBackoff {
			t.Fatalf("backoff(%d) = %s，超过上限 %s", attempt, value, dispatcher.MaxBackoff)
		}
		if previous != 0 && value < previous && value != dispatcher.MaxBackoff {
			t.Fatalf("backoff 必须单调不减：%s -> %s", previous, value)
		}
		previous = value
	}
}
