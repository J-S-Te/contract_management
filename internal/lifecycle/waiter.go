// Package lifecycle 提供进程退出时后台组件的 join 与排空契约。
//
// 后台投递循环使用进程信号 ctx 运行；收到 SIGTERM/SIGINT 后，主进程先停止接收
// 新请求，再取消投递 ctx，并在预算内等待各组件返回。组件内部在取消后仍必须用
// 独立 ctx 完成已认领批次的落库，否则行会停在 CLAIMED 等租约过期。
package lifecycle

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultDrainBudget 是等待后台组件退出的默认预算，可用
	// CONTRACT_SHUTDOWN_DRAIN_SECONDS 覆盖。
	DefaultDrainBudget = 20 * time.Second
	// DrainPersistTimeout 是取消后用独立 ctx 完成单批落库的上界，避免退出被拖死。
	DrainPersistTimeout = 5 * time.Second
)

// DrainBudget 解析 CONTRACT_SHUTDOWN_DRAIN_SECONDS（秒）；非法值或 <=0 回落到默认预算。
// 参数：raw 为环境变量原文，fallback 为默认预算。
// 返回值：解析出的预算，永不为 0 或负数。
func DrainBudget(raw string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

// Waiter 跟踪具名后台组件，并在退出预算内等待它们返回。
// 组件通过 Go 注册；Wait 返回超预算仍未退出的组件名，调用方记录日志后以非零码退出，
// 与 platform/Settlement 的停机契约一致：后台组件没停下来不能被当成正常停机。
type Waiter struct {
	budget time.Duration

	mu      sync.Mutex
	wg      sync.WaitGroup
	running map[string]int
}

// NewWaiter 构造 join 器；budget <= 0 时使用默认预算。
// 参数：budget 为等待后台组件退出的预算。
// 返回值：可用的 Waiter。
func NewWaiter(budget time.Duration) *Waiter {
	if budget <= 0 {
		budget = DefaultDrainBudget
	}
	return &Waiter{budget: budget, running: make(map[string]int)}
}

// Go 以名字注册并启动一个后台组件。
// 参数：name 为日志/告警用的组件名，run 为组件循环，必须在 ctx 取消后返回。
// 返回值：无。
func (w *Waiter) Go(name string, run func()) {
	w.mu.Lock()
	w.running[name]++
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer w.done(name)
		run()
	}()
}

// Wait 等待全部组件返回，最多等待预算时长。
// 参数：无。
// 返回值：超预算仍未退出的组件名（按字典序去重）；全部按时退出时返回 nil。
func (w *Waiter) Wait() []string {
	finished := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(finished)
	}()
	timer := time.NewTimer(w.budget)
	defer timer.Stop()
	select {
	case <-finished:
		return nil
	case <-timer.C:
		return w.unfinished()
	}
}

// LogUnfinished 在超预算时按统一格式记录未退出组件，供 API/worker 进程共用。
// 参数：logger 为进程日志，component 为进程名，budget 为本次预算。
// 返回值：全部组件都在预算内退出时为 true；超预算时为 false（已记录告警）。
// 调用方必须用该返回值决定退出码：false 表示后台组件没停下来，不能以 0 退出。
func (w *Waiter) LogUnfinished(logger *slog.Logger, component string, budget time.Duration) bool {
	unfinished := w.Wait()
	if len(unfinished) == 0 {
		return true
	}
	if logger != nil {
		logger.Error(component+" shutdown exceeded drain budget",
			"budget_seconds", budget.Seconds(), "components", unfinished)
	}
	return false
}

func (w *Waiter) done(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running[name] <= 1 {
		delete(w.running, name)
		return
	}
	w.running[name]--
}

func (w *Waiter) unfinished() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(w.running))
	for name := range w.running {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PersistContext 返回用于退出期间落库的 ctx：调用方 ctx 仍有效时原样返回，
// 保证正常路径行为不变；一旦取消则返回脱离取消、最多 DrainPersistTimeout 的 ctx，
// 使已认领批次仍能写入终态（MarkDelivered/MarkFailed），而不是停留在 CLAIMED。
// 参数：ctx 为投递循环的进程 ctx。
// 返回值：落库 ctx 与其取消函数；调用方必须调用取消函数。
func PersistContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return DrainContext(ctx)
}

// DrainContext 返回脱离父 ctx 取消、最多 DrainPersistTimeout 的 ctx，
// 用于 ctx 取消后继续完成已认领批次的投递与落库。
// 参数：ctx 为投递循环的进程 ctx。
// 返回值：排空 ctx 与其取消函数；调用方必须调用取消函数。
func DrainContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), DrainPersistTimeout)
}
