package auditoutbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// purgeStore 在 fakeStore 之上记录清理调用，用于验证保留期与分批循环；
// 不连接数据库，因此可在无 DSN 的环境里稳定执行。
type purgeStore struct {
	fakeStore
	purgeCalls   int
	purgeCutoff  []time.Time
	purgeBatches []int
	// remaining 是"还剩多少行可删"，每次调用按 batchSize 递减，模拟分批追平。
	remaining int64
	deleted   int64
	err       error
}

func (s *purgeStore) deletedTotal() int64 { return s.deleted }

func (s *purgeStore) PurgeDelivered(_ context.Context, cutoff time.Time, batchSize int) (int64, error) {
	s.purgeCalls++
	s.purgeCutoff = append(s.purgeCutoff, cutoff)
	s.purgeBatches = append(s.purgeBatches, batchSize)
	if s.err != nil {
		return 0, s.err
	}
	if s.remaining <= 0 {
		return 0, nil
	}
	deleted := int64(batchSize)
	if deleted > s.remaining {
		deleted = s.remaining
	}
	s.remaining -= deleted
	s.deleted += deleted
	return deleted, nil
}

// 保留期显式启用后，单轮清理必须分批（每批不超过上限）并一直追到返回 0。
func TestPurgeDeliveredBatchesUntilEmpty(t *testing.T) {
	t.Setenv(deliveredRetentionEnv, "")
	store := &purgeStore{remaining: 2500}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1"}
	cutoff := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	dispatcher.purgeDelivered(context.Background(), cutoff)

	// 2500 行按每批 1000 需要 3 次有删除的调用，再加一次返回 0 收尾。
	if store.purgeCalls != 4 {
		t.Fatalf("purgeCalls = %d，2500 行按每批 %d 需要 3 次删除 + 1 次收尾", store.purgeCalls, purgeBatchSize)
	}
	if store.deletedTotal() != 2500 {
		t.Fatalf("deleted = %d, want 2500", store.deletedTotal())
	}
	for i, batch := range store.purgeBatches {
		if batch != purgeBatchSize {
			t.Fatalf("batch[%d] = %d，单批不得超过 %d", i, batch, purgeBatchSize)
		}
	}
	for i, got := range store.purgeCutoff {
		if !got.Equal(cutoff) {
			t.Fatalf("cutoff[%d] = %s，必须原样使用保留期截止时间 %s", i, got, cutoff)
		}
	}
}

// 保留期配置 <=0 表示关闭清理：AUDIT_OUTBOX_DELIVERED_RETENTION_DAYS=0 时不得删除任何行。
func TestPurgeDisabledWhenRetentionNotPositive(t *testing.T) {
	for name, value := range map[string]string{"zero": "0", "negative": "-1"} {
		t.Setenv(deliveredRetentionEnv, value)
		if retention := DeliveredRetentionFromEnv(); retention > 0 {
			t.Fatalf("%s: retention = %s，<=0 必须关闭清理", name, retention)
		}
		// 关闭清理时 Run 循环里的调度判定必须为 false：既不查也不删。
		if purgeDue(DeliveredRetentionFromEnv(), time.Time{}, time.Now()) {
			t.Fatalf("%s: retention 关闭时不得调度清理", name)
		}
	}
}

// 缺省与非法取值都退回 defaultDeliveredRetention=0（关闭清理），绝不影响启动。
func TestDeliveredRetentionFromEnvDefaults(t *testing.T) {
	// 缺省口径必须是关闭：删除的是仅本地保存的已投递审计证据，保留期未经批准不得启用。
	if defaultDeliveredRetention != 0 {
		t.Fatalf("defaultDeliveredRetention = %s, want 0（缺省必须关闭清理）", defaultDeliveredRetention)
	}
	for name, value := range map[string]string{"unset": "", "invalid": "30d", "spaces": "  "} {
		t.Setenv(deliveredRetentionEnv, value)
		if retention := DeliveredRetentionFromEnv(); retention != defaultDeliveredRetention {
			t.Fatalf("%s: retention = %s, want %s（缺省/非法值必须关闭清理）", name, retention, defaultDeliveredRetention)
		}
	}
	t.Setenv(deliveredRetentionEnv, "7")
	if retention := DeliveredRetentionFromEnv(); retention != 7*24*time.Hour {
		t.Fatalf("retention = %s, want 168h", retention)
	}
}

// 缺省不清理：未配置 AUDIT_OUTBOX_DELIVERED_RETENTION_DAYS 时 Run 循环既不查也不删。
func TestPurgeDisabledByDefault(t *testing.T) {
	t.Setenv(deliveredRetentionEnv, "")
	if retention := DeliveredRetentionFromEnv(); retention != 0 {
		t.Fatalf("retention = %s，缺省必须关闭清理", retention)
	}
	// 关闭清理时调度判定必须为 false：这是 Run 循环发起删除的唯一前提。
	if purgeDue(DeliveredRetentionFromEnv(), time.Time{}, time.Now()) {
		t.Fatal("缺省保留期下不得调度清理")
	}
	// 端到端确认：缺省配置下跑一轮 Run，即使有待清理的已投递行也不得发起删除。
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	store := &purgeStore{remaining: 2500}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dispatcher.Run(ctx)
	if store.purgeCalls != 0 || store.deletedTotal() != 0 {
		t.Fatalf("purgeCalls = %d, deleted = %d，缺省关闭时不得删除任何行", store.purgeCalls, store.deletedTotal())
	}
}

// 保留期与间隔共同决定是否调度清理：正数保留期在 1 小时后到期，关闭时永不到期。
func TestPurgeDueHonoursRetentionAndInterval(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	// 用显式正数保留期验证「首次清理 / 间隔内不重复 / 满间隔再清理」；
	// 缺省值为 0（关闭），不能再用它验证正向调度。
	enabledRetention := 30 * 24 * time.Hour
	if !purgeDue(enabledRetention, time.Time{}, now) {
		t.Fatal("首次轮次必须清理")
	}
	if purgeDue(enabledRetention, now.Add(-purgeInterval+time.Second), now) {
		t.Fatal("间隔内不得重复清理")
	}
	if !purgeDue(enabledRetention, now.Add(-purgeInterval), now) {
		t.Fatal("满一个间隔后必须清理")
	}
	if purgeDue(defaultDeliveredRetention, time.Time{}, now) {
		t.Fatalf("defaultDeliveredRetention = %s 必须关闭清理", defaultDeliveredRetention)
	}
	for _, retention := range []time.Duration{0, -time.Second} {
		if purgeDue(retention, time.Time{}, now) {
			t.Fatalf("retention = %s 必须关闭清理", retention)
		}
	}
}

// 清理失败只记日志，不能吞掉投递主流程，也不能无限重试同一批。
func TestPurgeDeliveredStopsOnError(t *testing.T) {
	store := &purgeStore{err: errors.New("mysql: connection refused")}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1"}
	dispatcher.purgeDelivered(context.Background(), time.Now().UTC())
	if store.purgeCalls != 1 {
		t.Fatalf("purgeCalls = %d，失败后必须停止本轮清理", store.purgeCalls)
	}
}

// ctx 取消后立即退出，不再发起新批次。
func TestPurgeDeliveredStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &purgeStore{remaining: 10}
	dispatcher := &Dispatcher{Store: store, WorkerID: "w1"}
	dispatcher.purgeDelivered(ctx, time.Now().UTC())
	if store.purgeCalls != 0 {
		t.Fatalf("purgeCalls = %d，ctx 已取消时不得再发起删除", store.purgeCalls)
	}
}

// Store 没有清理能力时（例如测试替身或未来实现）不得 panic。
func TestPurgeDeliveredSkipsStoreWithoutPurger(t *testing.T) {
	dispatcher := &Dispatcher{Store: &fakeStore{}, WorkerID: "w1"}
	dispatcher.purgeDelivered(context.Background(), time.Now().UTC())
}

// PurgeDelivered 缺少 DB 时必须返回错误而不是静默成功。
func TestPurgeDeliveredRequiresDB(t *testing.T) {
	if _, err := (&GORMStore{}).PurgeDelivered(context.Background(), time.Now().UTC(), 10); err == nil {
		t.Fatal("缺少 DB 时必须返回错误")
	}
	if _, _, err := (&GORMStore{}).DeliveryStatusCounts(context.Background()); err == nil {
		t.Fatal("缺少 DB 时计数必须返回错误")
	}
}
