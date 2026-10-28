package settlement

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type financialDeliveryTestStore struct {
	delivery          Delivery
	delivered, failed string
	dead              bool
	claims            int
}

func (s *financialDeliveryTestStore) ClaimFinancialEffectiveDelivery(context.Context) (Delivery, bool, error) {
	s.claims++
	return s.delivery, true, nil
}
func (s *financialDeliveryTestStore) MarkFinancialEffectiveDelivered(_ context.Context, id string) error {
	s.delivered = id
	return nil
}
func (s *financialDeliveryTestStore) MarkFinancialEffectiveFailed(_ context.Context, id, _ string, _ uint, dead bool) error {
	s.failed, s.dead = id, dead
	return nil
}

type financialRoundTrip func(*http.Request) (*http.Response, error)

func (f financialRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFinancialDispatcherUsesExactSettlementEndpointAndFrozenPayload(t *testing.T) {
	payload := `{"event_id":"event","event_type":"contract.financial_effective.v1","tenant_id":"tenant","contract":{"id":"contract","version":5,"amount":"100.00","currency":"CNY","payment_terms":[{"installment_no":1,"due_date":"2026-12-01","amount":"100.00"}]}}`
	store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(payload), Attempts: 1}}
	calls := 0
	client := &http.Client{Transport: financialRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != payload || r.Method != http.MethodPost || r.URL.String() != "http://settlement-api:8085/internal/v1/settlement/events/contracts" {
			t.Fatalf("unexpected financial delivery request: %v", err)
		}
		if r.Header.Get("Authorization") != "Bearer test-only-machine-token" || r.Header.Get("X-Financial-Effective-Tenant-ID") != "tenant" || r.Header.Get("X-Financial-Effective-Delivery-ID") != "event" {
			t.Fatal("machine binding metadata missing")
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Header: make(http.Header)}, nil
	})}
	d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 3, Client: client, TokenSource: func(context.Context) (string, error) { return "test-only-machine-token", nil }}
	if err := d.dispatchOne(context.Background()); err != nil || calls != 1 || store.delivered != "event" || store.failed != "" {
		t.Fatal("financial event was not delivered exactly once", err)
	}
}

func TestFinancialDispatcherPersistsTokenAndReceiverFailures(t *testing.T) {
	for _, tokenFailure := range []bool{true, false} {
		store := &financialDeliveryTestStore{delivery: Delivery{ID: "event", TenantID: "tenant", Payload: []byte(`{}`), Attempts: 3}}
		calls := 0
		client := &http.Client{Transport: financialRoundTrip(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader("invalid schedule")), Header: make(http.Header)}, nil
		})}
		d := &Dispatcher{Store: store, BaseURL: "http://settlement-api:8085", MaxAttempts: 3, Client: client, TokenSource: func(context.Context) (string, error) {
			if tokenFailure {
				return "", errors.New("test token endpoint unavailable")
			}
			return "test-only-machine-token", nil
		}}
		if err := d.dispatchOne(context.Background()); err == nil || store.failed != "event" || !store.dead || store.delivered != "" || tokenFailure && calls != 0 || !tokenFailure && calls != 1 {
			t.Fatal("failure was swallowed or falsely acknowledged")
		}
	}
}

// TestResponsibilityDispatcherUsesIndependentEndpoint 验证独立责任事件走责任端点。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityDispatcherUsesIndependentEndpoint(t *testing.T) {
	store := &financialDeliveryTestStore{delivery: Delivery{ID: "R1", Payload: []byte(`{"event_type":"contract.responsibility_changed.v1"}`)}}
	client := &http.Client{Transport: financialRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/internal/v1/settlement/events/contract-responsibility" {
			t.Fatalf("责任事件端点错误：%s", r.URL.Path)
		}
		return &http.Response{StatusCode: 202, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}
	if err := (&Dispatcher{Store: store, BaseURL: "http://settlement", Client: client}).dispatchOne(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type responsibilityDeliveryStore struct {
	financialDeliveryTestStore
	responsibilityMarked, responsibilityFailure string
}

// ClaimContractResponsibilityDelivery 提供独立责任队列行。
// 参数：ctx 为测试请求上下文。
// 返回值：冻结的责任载荷、存在标志和错误。
func (s *responsibilityDeliveryStore) ClaimContractResponsibilityDelivery(ctx context.Context) (Delivery, bool, error) {
	return s.delivery, true, nil
}

// ClaimFinancialEffectiveDelivery 模拟当前无财务投递。
// 参数：ctx 为请求上下文。
// 返回值：空财务队列。
func (s *responsibilityDeliveryStore) ClaimFinancialEffectiveDelivery(ctx context.Context) (Delivery, bool, error) {
	return Delivery{}, false, nil
}

// MarkContractResponsibilityDelivered 观察责任成功状态写入独立队列。
// 参数：ctx 为上下文，id 为责任事件号。
// 返回值：无错误。
func (s *responsibilityDeliveryStore) MarkContractResponsibilityDelivered(ctx context.Context, id string) error {
	s.responsibilityMarked = id
	return nil
}

// MarkContractResponsibilityFailed 观察责任失败和死信持久化分支。
// 参数：ctx 为上下文，id 为事件号，message 为错误摘要，attempts 为尝试次数，dead 为死信标志。
// 返回值：无错误。
func (s *responsibilityDeliveryStore) MarkContractResponsibilityFailed(ctx context.Context, id, message string, attempts uint, dead bool) error {
	s.responsibilityFailure = id
	s.dead = dead
	return nil
}

// ReconcileContractResponsibilities 模拟已存在全部责任基线。
// 参数：ctx 为请求上下文。
// 返回值：零条补建和无错误。
func (s *responsibilityDeliveryStore) ReconcileContractResponsibilities(ctx context.Context) (int, error) {
	return 0, nil
}

// TestResponsibilityDispatcherPersistsIndependentFailure 验证worker实际认领责任队列并写独立重试结果。
// 参数：t 为测试上下文。
// 返回值：无。
func TestResponsibilityDispatcherPersistsIndependentFailure(t *testing.T) {
	for _, status := range []int{202, 503} {
		store := &responsibilityDeliveryStore{financialDeliveryTestStore: financialDeliveryTestStore{delivery: Delivery{ID: "responsibility", Attempts: 3, Payload: []byte(`{"event_type":"contract.responsibility_changed.v1"}`)}}}
		d := &Dispatcher{Store: store, BaseURL: "http://settlement", MaxAttempts: 3, Client: &http.Client{Transport: financialRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/internal/v1/settlement/events/contract-responsibility" {
				t.Fatal("责任队列走错端点")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})}}
		err := d.dispatchOne(context.Background())
		if status == 202 && (err != nil || store.responsibilityMarked != "responsibility") {
			t.Fatalf("责任成功未写回：%v", err)
		}
		if status == 503 && (err == nil || store.responsibilityFailure != "responsibility" || !store.dead) {
			t.Fatalf("责任失败未持久化：%v", err)
		}
		if store.delivered != "" || store.failed != "" {
			t.Fatal("责任事件污染了财务投递状态")
		}
	}
}

type startupReconcileStore struct {
	financialDeliveryTestStore
	reconciles int
	cancel     context.CancelFunc
}

// ReconcileFinancialEffectives 模拟启动时依赖暂不可用。
// 参数：ctx 为worker生命周期。
// 返回值：首次依赖失败，恢复后补建成功并停止测试worker。
func (s *startupReconcileStore) ReconcileFinancialEffectives(ctx context.Context) (int, error) {
	s.reconciles++
	if s.reconciles == 1 {
		return 0, errors.New("startup dependency unavailable")
	}
	s.cancel()
	return 1, nil
}

// ClaimFinancialEffectiveDelivery 模拟等待补建期间没有队列行。
// 参数：ctx 为worker生命周期。
// 返回值：空队列。
func (s *startupReconcileStore) ClaimFinancialEffectiveDelivery(ctx context.Context) (Delivery, bool, error) {
	return Delivery{}, false, nil
}

// TestDispatcherRetriesFailedStartupReconciliation 验证启动补建失败不会永久漏同步旧合同。
// 参数：t 为测试上下文。
// 返回值：无。
func TestDispatcherRetriesFailedStartupReconciliation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	store := &startupReconcileStore{cancel: cancel}
	d := &Dispatcher{Store: store, Poll: time.Millisecond}
	d.Run(ctx)
	if store.reconciles < 2 {
		t.Fatalf("启动失败后未重试补建：%d", store.reconciles)
	}
}
