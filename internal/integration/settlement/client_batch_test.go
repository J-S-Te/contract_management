package settlement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// batchFinancialTestStore 模拟支持批量认领的财务仓储。
// 它只实现结算 Store + 批量接口，不实现 responsibilityStore，
// 从而把断言集中在财务队列的批量语义上。
type batchFinancialTestStore struct {
	mu        sync.Mutex
	pending   []Delivery
	limits    []int
	delivered []string
	failed    []string
	claimErr  error
	// partialClaim 非空时模拟仓储只返回前 N 条（模拟本轮积压被其它 worker 抢走）。
	partialClaim int
}

// ClaimFinancialEffectiveDelivery 让旧单条接口保持可用；批量路径存在时不应被调用。
func (s *batchFinancialTestStore) ClaimFinancialEffectiveDelivery(context.Context) (Delivery, bool, error) {
	return Delivery{}, false, errors.New("single claim must not be used when batch claim is available")
}

// ClaimFinancialEffectiveDeliveries 按上限返回待投递行，并记录调用上限。
func (s *batchFinancialTestStore) ClaimFinancialEffectiveDeliveries(_ context.Context, limit int) ([]Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = append(s.limits, limit)
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	size := limit
	if s.partialClaim > 0 && size > s.partialClaim {
		size = s.partialClaim
	}
	if size > len(s.pending) {
		size = len(s.pending)
	}
	claimed := append([]Delivery(nil), s.pending[:size]...)
	s.pending = s.pending[size:]
	return claimed, nil
}

// MarkFinancialEffectiveDelivered 记录逐行成功回写。
func (s *batchFinancialTestStore) MarkFinancialEffectiveDelivered(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, id)
	return nil
}

// MarkFinancialEffectiveFailed 记录逐行失败回写（退避/死信各自由仓储决定）。
func (s *batchFinancialTestStore) MarkFinancialEffectiveFailed(_ context.Context, id, _ string, _ uint, _ bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = append(s.failed, id)
	return nil
}

// seedBatchDeliveries 生成 n 条带事件的待投递行。
func seedBatchDeliveries(n int) []Delivery {
	deliveries := make([]Delivery, 0, n)
	for index := 0; index < n; index++ {
		deliveries = append(deliveries, Delivery{
			ID:       fmt.Sprintf("event-%03d", index),
			TenantID: "tenant",
			Payload:  []byte(`{"event_type":"contract.financial_effective.v1"}`),
			Attempts: 1,
		})
	}
	return deliveries
}

// TestBatchDispatcherDrainsBacklogWithinBoundedLimit 验证单轮按有界批量领取，
// 不再“每 2s 只投 1 条”：默认批量 25、上限 200，且一次 dispatch 内逐条回写成功。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBatchDispatcherDrainsBacklogWithinBoundedLimit(t *testing.T) {
	store := &batchFinancialTestStore{pending: seedBatchDeliveries(60)}
	client := &http.Client{Transport: financialRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Header: make(http.Header)}, nil
	})}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement", MaxAttempts: 3, Client: client}

	if got := d.batchSize(); got != defaultDeliveryBatchSize {
		t.Fatalf("默认批量=%d want %d", got, defaultDeliveryBatchSize)
	}
	if err := d.dispatch(context.Background(), d.batchSize()); err != nil {
		t.Fatal(err)
	}
	if len(store.delivered) != defaultDeliveryBatchSize || len(store.pending) != 60-defaultDeliveryBatchSize {
		t.Fatalf("单轮投递=%d 剩余=%d，未按有界批量清理积压", len(store.delivered), len(store.pending))
	}
	if len(store.limits) != 1 || store.limits[0] != defaultDeliveryBatchSize {
		t.Fatalf("认领上限=%v want [%d]", store.limits, defaultDeliveryBatchSize)
	}

	// 超界批量必须收敛到上限，避免一次认领退化成无界工作。
	d.BatchSize = maxDeliveryBatchSize + 500
	if got := d.batchSize(); got != maxDeliveryBatchSize {
		t.Fatalf("超大批量未被封顶：%d", got)
	}
	d.BatchSize = -3
	if got := d.batchSize(); got != defaultDeliveryBatchSize {
		t.Fatalf("非法批量未回退默认：%d", got)
	}

	// 认领少于上限（例如被其它 worker 抢走积压行）时必须按实际认领数投递。
	store.partialClaim = 2
	before := len(store.delivered)
	if err := d.dispatch(context.Background(), d.batchSize()); err != nil {
		t.Fatal(err)
	}
	if got := len(store.delivered) - before; got != 2 {
		t.Fatalf("部分认领投递=%d want 2", got)
	}
}

// TestBatchDispatcherIsolatesPerEventFailures 验证批量中单条失败只回写该行，
// 同批其它事件照常投递（顺序不参与语义，失败不扩散为整批停摆）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBatchDispatcherIsolatesPerEventFailures(t *testing.T) {
	store := &batchFinancialTestStore{pending: seedBatchDeliveries(4)}
	var (
		mu    sync.Mutex
		calls []string
	)
	client := &http.Client{Transport: financialRoundTrip(func(r *http.Request) (*http.Response, error) {
		id := r.Header.Get("X-Financial-Effective-Delivery-ID")
		mu.Lock()
		calls = append(calls, id)
		mu.Unlock()
		if id == "event-001" || id == "event-002" {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("unavailable")), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement", MaxAttempts: 5, Client: client}

	err := d.dispatch(context.Background(), 10)
	if err == nil {
		t.Fatal("批量投递失败被静默吞掉")
	}
	if len(calls) != 4 {
		t.Fatalf("批量失败后中断了同批其它事件：calls=%v", calls)
	}
	if got := strings.Join(store.delivered, ","); got != "event-000,event-003" {
		t.Fatalf("成功回写=%s", got)
	}
	if got := strings.Join(store.failed, ","); got != "event-001,event-002" {
		t.Fatalf("失败回写=%s", got)
	}
}

// TestBatchDispatcherSurfacesClaimError 验证认领失败原样返回，不被当作“队列为空”。
// 参数：t 为测试上下文。
// 返回值：无。
func TestBatchDispatcherSurfacesClaimError(t *testing.T) {
	claimFailure := errors.New("claim transaction failed")
	store := &batchFinancialTestStore{claimErr: claimFailure}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement"}
	if err := d.dispatch(context.Background(), 5); !errors.Is(err, claimFailure) {
		t.Fatalf("认领失败未返回：%v", err)
	}
}

// periodicReconcileStore 记录补建次数并在每次成功后返回积压计数，
// 用于证明补建是周期执行而不是“只在启动跑一次”。
type periodicReconcileStore struct {
	batchFinancialTestStore
	mu         sync.Mutex
	reconciles int
	backlogs   int
	backlog    int
}

// ClaimFinancialEffectiveDeliveries 让批量认领始终为空，把注意力放在补建节奏上。
func (s *periodicReconcileStore) ClaimFinancialEffectiveDeliveries(context.Context, int) ([]Delivery, error) {
	return nil, nil
}

// ReconcileFinancialEffectives 记录补建调用次数。
func (s *periodicReconcileStore) ReconcileFinancialEffectives(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciles++
	return 0, nil
}

// FinancialEffectiveBacklog 记录缺口计数查询，用于验证告警可见性。
func (s *periodicReconcileStore) FinancialEffectiveBacklog(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backlogs++
	return s.backlog, nil
}

// TestDispatcherReconcilesPeriodicallyAfterSuccess 验证补建成功后仍按周期继续，
// 且“已生效但无财务事件”的缺口会以告警形式可见（不是静默分支）。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDispatcherReconcilesPeriodicallyAfterSuccess(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := &periodicReconcileStore{backlog: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	d := &Dispatcher{Store: store, Poll: 5 * time.Millisecond, ReconcileEvery: 20 * time.Millisecond, Logger: logger}
	d.Run(ctx)

	if store.reconciles < 3 {
		t.Fatalf("补建只跑了 %d 次，未按周期重入", store.reconciles)
	}
	if store.backlogs == 0 {
		t.Fatal("未查询财务事件缺口，静默缺口不可观测")
	}
	if !strings.Contains(logs.String(), "effective contracts without financial effective event") {
		t.Fatalf("缺口未产生告警日志：%s", logs.String())
	}
}

// failingReconcileStore 模拟补建存储持续失败。
type failingReconcileStore struct {
	batchFinancialTestStore
	mu         sync.Mutex
	reconciles int
}

// ClaimFinancialEffectiveDeliveries 让批量认领始终为空。
func (s *failingReconcileStore) ClaimFinancialEffectiveDeliveries(context.Context, int) ([]Delivery, error) {
	return nil, nil
}

// ReconcileFinancialEffectives 返回可识别的存储错误。
func (s *failingReconcileStore) ReconcileFinancialEffectives(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconciles++
	return 0, errors.New("reconcile storage unavailable")
}

// TestDispatcherRetriesFailedReconcileEveryTick 验证补建失败立即在下一轮重试，
// 且失败被记录为错误而不是被当作“没有历史欠账”。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDispatcherRetriesFailedReconcileEveryTick(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := &failingReconcileStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	d := &Dispatcher{Store: store, Poll: 5 * time.Millisecond, ReconcileEvery: time.Hour, Logger: logger}
	d.Run(ctx)

	if store.reconciles < 3 {
		t.Fatalf("补建失败后未逐轮重试：%d", store.reconciles)
	}
	if !strings.Contains(logs.String(), "reconcile settlement deliveries") {
		t.Fatalf("补建失败未记录错误日志：%s", logs.String())
	}
}
