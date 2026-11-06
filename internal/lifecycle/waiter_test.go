package lifecycle

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 预算来自 CONTRACT_SHUTDOWN_DRAIN_SECONDS；非法值或 <=0 必须回落到默认预算，
// 不能让一个手误的配置把进程退出等待变成 0 秒或无限阻塞。
func TestDrainBudgetFallsBackOnInvalidValues(t *testing.T) {
	const fallback = 20 * time.Second
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{raw: "25", want: 25 * time.Second},
		{raw: " 15 ", want: 15 * time.Second},
		{raw: "1", want: time.Second},
		{raw: "", want: fallback},
		{raw: "0", want: fallback},
		{raw: "-5", want: fallback},
		{raw: "abc", want: fallback},
	}
	for _, c := range cases {
		if got := DrainBudget(c.raw, fallback); got != c.want {
			t.Fatalf("DrainBudget(%q) = %s，期望 %s", c.raw, got, c.want)
		}
	}
}

// 全部后台组件在预算内退出时必须立即返回，空名单。
func TestWaiterReturnsNilWhenAllComponentsExit(t *testing.T) {
	waiter := NewWaiter(2 * time.Second)
	release := make(chan struct{})
	close(release)
	waiter.Go("quick-a", func() { <-release })
	waiter.Go("quick-b", func() { <-release })

	if stuck := waiter.Wait(); len(stuck) != 0 {
		t.Fatalf("stuck = %v，全部退出后不应有未完成组件", stuck)
	}
}

// 超预算必须返回未退出组件名单（按名字排序），而不是 panic 或无限等待。
func TestWaiterReportsUnfinishedComponentsAfterBudget(t *testing.T) {
	waiter := NewWaiter(50 * time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	waiter.Go("audit-outbox", func() { <-release })
	waiter.Go("crm-link-outbox", func() { <-release })
	waiter.Go("notification-outbox", func() {})

	started := time.Now()
	stuck := waiter.Wait()
	elapsed := time.Since(started)

	if elapsed < 50*time.Millisecond {
		t.Fatalf("elapsed = %s，必须等待预算耗尽后才能返回名单", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %s，超预算必须及时返回", elapsed)
	}
	if len(stuck) != 2 || stuck[0] != "audit-outbox" || stuck[1] != "crm-link-outbox" {
		t.Fatalf("stuck = %v，必须列出未退出的组件名", stuck)
	}
}

// 同名组件并发注册时，先退出的一路不能把仍在跑的同名组件从名单里抹掉。
func TestWaiterKeepsNameUntilLastInstanceExits(t *testing.T) {
	waiter := NewWaiter(50 * time.Millisecond)
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	defer close(releaseFirst)
	defer close(releaseSecond)
	waiter.Go("same-name", func() { <-releaseFirst })
	waiter.Go("same-name", func() { <-releaseSecond })

	if stuck := waiter.Wait(); len(stuck) != 1 || stuck[0] != "same-name" {
		t.Fatalf("stuck = %v", stuck)
	}
}

// 全部组件在预算内退出时 LogUnfinished 必须返回 ok=true 且不写告警：
// 调用方据此保持 0 退出码。
func TestLogUnfinishedReportsOKWhenAllComponentsExit(t *testing.T) {
	waiter := NewWaiter(2 * time.Second)
	release := make(chan struct{})
	close(release)
	waiter.Go("quick-a", func() { <-release })

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	if ok := waiter.LogUnfinished(logger, "contract API", 2*time.Second); !ok {
		t.Fatalf("LogUnfinished ok = false，全部退出时必须返回 true")
	}
	if logs.Len() != 0 {
		t.Fatalf("logs = %q，全部退出时不得写超预算告警", logs.String())
	}
}

// 超预算时 LogUnfinished 必须返回 ok=false（调用方据此非零退出），
// 且日志文案与未退出名单保持原有格式。
func TestLogUnfinishedReportsNotOKWhenBudgetExceeded(t *testing.T) {
	waiter := NewWaiter(50 * time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	waiter.Go("audit-outbox", func() { <-release })

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	if ok := waiter.LogUnfinished(logger, "contract API", 50*time.Millisecond); ok {
		t.Fatalf("LogUnfinished ok = true，超预算时必须返回 false 以供非零退出")
	}
	line := logs.String()
	for _, want := range []string{
		"contract API shutdown exceeded drain budget",
		`"budget_seconds":0.05`,
		"audit-outbox",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("日志 %q 缺少 %q，超预算告警文案不得变化", line, want)
		}
	}
}

// nil logger 时也必须按等待结果返回退出判定，不能因为没日志就吞掉超预算。
func TestLogUnfinishedHandlesNilLogger(t *testing.T) {
	waiter := NewWaiter(50 * time.Millisecond)
	release := make(chan struct{})
	defer close(release)
	waiter.Go("audit-outbox", func() { <-release })

	if ok := waiter.LogUnfinished(nil, "contract API", 50*time.Millisecond); ok {
		t.Fatal("LogUnfinished ok = true，nil logger 时超预算仍须返回 false")
	}
}

// 取消后完成落库的 ctx 必须脱离父 ctx 的取消，并带 5s 上界。
func TestPersistContextDetachesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	persistCtx, stop := PersistContext(ctx)
	defer stop()
	if err := persistCtx.Err(); err != nil {
		t.Fatalf("PersistContext 返回了已取消的 ctx: %v", err)
	}
	deadline, ok := persistCtx.Deadline()
	if !ok {
		t.Fatal("排空落库 ctx 必须有超时上界")
	}
	if left := time.Until(deadline); left <= 0 || left > DrainPersistTimeout {
		t.Fatalf("排空落库上界 = %s，期望不超过 %s", left, DrainPersistTimeout)
	}

	// 正常路径必须原样复用调用方 ctx，不改变既有行为。
	live := context.Background()
	same, stopLive := PersistContext(live)
	defer stopLive()
	if same != live {
		t.Fatal("ctx 未取消时不得替换调用方 ctx")
	}
}
